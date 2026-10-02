package sftp

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bethropolis/kcd/internal/device"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
	"github.com/bethropolis/kcd/internal/plugin"
)

// sshUserPattern allows the generated Android SFTP usernames (alphanumerics,
// underscore, dot, hyphen) while rejecting anything starting with '-' —
// sshfs would parse that as an option flag (e.g. -oProxyCommand=...),
// yielding local command execution.
var sshUserPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// sshHostPattern allows IPs (validated separately) and plain hostnames
// (.local, LAN names). Anything else — flags, spaces, shell metachars,
// userinfo (@) — is rejected.
var sshHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// buildSSHFSArgs validates the phone-provided (or IPC-provided) remote
// parameters and constructs the sshfs argv. Validation (not a "--"
// separator — unsupported by older sshfs 2.x) is what prevents option
// injection: no validated value can begin with '-', so sshfs/fuse option
// parsing can never reinterpret remoteRoot as a flag like -oProxyCommand.
func buildSSHFSArgs(body SftpBody, remotePath, mountPoint string, uid, gid int, keepaliveInterval, keepaliveCount int, extraOpts []string, readOnly bool) ([]string, error) {
	if !sshUserPattern.MatchString(body.User) || len(body.User) > 64 {
		return nil, fmt.Errorf("sftp: refusing suspicious ssh user %q", body.User)
	}
	if body.IP == "" || strings.HasPrefix(body.IP, "-") {
		return nil, fmt.Errorf("sftp: refusing suspicious ssh host %q", body.IP)
	}
	if net.ParseIP(body.IP) == nil && (!sshHostPattern.MatchString(body.IP) || len(body.IP) > 253) {
		return nil, fmt.Errorf("sftp: refusing invalid ssh host %q", body.IP)
	}
	port, err := strconv.Atoi(strings.TrimSpace(body.Port.String()))
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("sftp: refusing invalid ssh port %q", body.Port.String())
	}
	if remotePath == "" || strings.HasPrefix(remotePath, "-") {
		return nil, fmt.Errorf("sftp: refusing suspicious remote path %q", remotePath)
	}
	remotePath = filepath.Clean(remotePath)
	remoteRoot := fmt.Sprintf("%s@%s:%s", body.User, body.IP, remotePath)

	args := []string{
		remoteRoot,
		mountPoint,
		"-p", strconv.Itoa(port),
		"-s",
		"-F", "/dev/null",
		"-o", "password_stdin",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "reconnect",
		"-o", "ServerAliveInterval=" + strconv.Itoa(keepaliveInterval),
		"-o", "ServerAliveCountMax=" + strconv.Itoa(keepaliveCount),
		"-o", "auto_cache",
		"-o", "kernel_cache",
		"-o", "uid=" + strconv.Itoa(uid),
		"-o", "gid=" + strconv.Itoa(gid),
	}

	// Read-only is set before ExtraSshfsOpts so an explicit operator override
	// in the config can still force a writable mount.
	if readOnly {
		args = append(args, "-o", "ro")
	}

	// ExtraSshfsOpts comes from the local operator config, not the phone —
	// passed through as-is.
	for _, opt := range extraOpts {
		args = append(args, "-o", opt)
	}
	return args, nil
}

// sshfsHint maps an sshfs failure message to an actionable hint, or "" when
// the cause is not one we can advise about.
//
// The FUSE hint is deliberately narrow. It used to fire on any message
// containing "fusermount", which also matched errors that have nothing to do
// with /etc/fuse.conf — most visibly re-running sshfs onto an already live
// mountpoint, whose message names fusermount3 but is caused by the duplicate
// mount. Telling a user to edit fuse.conf there sends them down a dead end.
// mountWithBody is now idempotent, but precision here is still worth having.
func sshfsHint(msg string) string {
	switch {
	case strings.Contains(msg, "Operation not permitted"),
		strings.Contains(msg, "user_allow_other"),
		strings.Contains(msg, "Permission denied"):
		return "Hint: FUSE requires user_allow_other in /etc/fuse.conf.\nRun: sudo sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf"
	case strings.Contains(msg, "sshfs: not found"),
		strings.Contains(msg, "executable file not found"):
		return "Hint: sshfs is not installed.\nInstall: sudo apt install sshfs  (or the equivalent for your distro)"
	default:
		return ""
	}
}

// mountWithBody performs the sshfs mount and returns the local browse path.
// volumePath specifies which storage volume to mount. If empty, the first
// available volume is selected automatically.
func (p *SftpPlugin) mountWithBody(ctx context.Context, deviceID string, body SftpBody, volumePath string, readOnly bool) (string, error) {
	mountPoint := p.mountPointFor(deviceID)
	// Warned before the reuse check as well as after it: a user who already
	// has a mount at a hazardous location still needs telling, and it is
	// logged once per location, so hoisting it costs nothing.
	p.warnIfInBulkDeletableDir(mountPoint)

	// Idempotent. Re-running sshfs onto a live mountpoint fails with
	// "fusermount3: failed to access mountpoint ... Permission denied", which
	// reads like a FUSE permissions problem and is not one. Returning the
	// existing mount point also makes a repeated request a cheap way to
	// re-open the file manager.
	if existing := p.MountedPath(deviceID); existing != "" {
		p.logger.Info("SFTP already mounted, reusing mount point",
			log.String("device_id", deviceID),
			log.String("mount_point", existing),
		)
		// Mounting is idempotent, so a mode request cannot be applied to an
		// existing mount. Say what the mount actually is rather than appearing
		// to honour the flag.
		if existingRO := p.mountIsReadOnly(deviceID); existingRO != readOnly {
			p.logger.Info("read-only mode differs from the existing mount and cannot be changed in place; unmount first",
				log.String("device_id", deviceID),
				log.Bool("existing_read_only", existingRO),
				log.Bool("requested_read_only", readOnly),
			)
		}
		p.autoOpen(existing)
		return existing, nil
	}

	if err := os.MkdirAll(mountPoint, 0700); err != nil {
		return "", fmt.Errorf("create mount point %s: %w", mountPoint, err)
	}

	// Determine the remote path on the Android device.
	// The Android SFTP server exposes the real filesystem at "/".
	// Listing "/" via sshfs fails because it contains permission-denied
	// entries (/proc, /sys). Instead, mount directly to a storage
	// volume (e.g. /storage/emulated/0) which is guaranteed browsable.
	// If a specific volumePath is provided, use it; otherwise auto-select
	// the first available volume.
	remotePath := volumePath
	if remotePath == "" {
		if len(body.MultiPaths) > 0 {
			remotePath = body.MultiPaths[0]
		} else if body.Path != "" && body.Path != "/" {
			remotePath = body.Path
		}
	}
	args, err := buildSSHFSArgs(body, remotePath, mountPoint, os.Getuid(), os.Getgid(), p.cfg.KeepaliveIntervalSecs, p.cfg.KeepaliveCount, p.cfg.ExtraSshfsOpts, readOnly)
	if err != nil {
		_ = os.Remove(mountPoint)
		return "", err
	}

	cmd := exec.CommandContext(ctx, "sshfs", args...)
	cmd.Stdin = strings.NewReader(body.Password + "\n")

	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(mountPoint)
		msg := strings.TrimSpace(string(out))
		errMsg := fmt.Sprintf("sshfs failed: %v\n%s", err, msg)
		if hint := sshfsHint(msg); hint != "" {
			errMsg += "\n\n" + hint
		}
		return "", fmt.Errorf("%s", errMsg)
	}

	// The mount point now IS the storage volume root, so the user browses
	// directly to the mount point — no extra navigation needed.
	browsePath := mountPoint

	// Track the mount point so Unmount() can call fusermount, and its mode so a
	// later --ro request against this mount can report the truth.
	p.mu.Lock()
	p.mountPoints[deviceID] = mountPoint
	p.mountReadOnly[deviceID] = readOnly
	p.mu.Unlock()

	// Find and track the sshfs daemon PID for graceful shutdown.
	if pid, err := findSSHFSPID(mountPoint); err == nil {
		p.mu.Lock()
		p.mountPIDs[deviceID] = pid
		p.mu.Unlock()
		p.logger.Debug("tracking sshfs PID", log.Int("pid", pid))
	} else {
		p.logger.Debug("could not find sshfs PID", log.Error(err))
	}

	p.logger.Info("SFTP mounted",
		log.String("mount_point", mountPoint),
		log.String("browse_path", browsePath),
	)
	p.publishMounted(deviceID, mountPoint, volumePath)

	p.autoOpen(browsePath)

	return browsePath, nil
}

// publishMounted announces a completed mount so clients can render a truthful
// mount toggle without inspecting /proc themselves.
func (p *SftpPlugin) publishMounted(deviceID, mountPoint, volumePath string) {
	if p.bus == nil {
		return
	}
	payload := map[string]any{"mountPoint": mountPoint}
	if volumePath != "" {
		payload["volume"] = volumePath
	}
	p.bus.Publish(events.TypeSftpMounted, deviceID, payload)
}

func (p *SftpPlugin) publishUnmounted(deviceID, mountPoint string) {
	if p.bus == nil {
		return
	}
	p.bus.Publish(events.TypeSftpUnmounted, deviceID, map[string]any{"mountPoint": mountPoint})
}

// autoOpen opens a browse path in the configured file manager, best effort.
// It runs the spawn in a goroutine so a slow or hung file manager never
// blocks the caller's mount or unmount path.
func (p *SftpPlugin) autoOpen(browsePath string) {
	if !p.cfg.AutoOpen {
		return
	}
	cmd := p.cfg.OpenCommand
	if cmd == "" {
		cmd = "xdg-open"
	}
	go func() {
		if err := exec.CommandContext(context.Background(), cmd, browsePath).Start(); err != nil {
			p.logger.Debug("auto-open failed", log.String("command", cmd), log.Error(err))
		}
	}()
}

func (p *SftpPlugin) OnConnect(_ device.Sender) {}

func (p *SftpPlugin) OnDisconnect(dev device.Sender) {
	deviceID := dev.ID()
	// Via MountedPath, so a mount adopted from the kernel is cleaned up too
	// rather than surviving every disconnect untouched.
	if p.MountedPath(deviceID) != "" {
		p.logger.Info("device disconnected, cleaning up SFTP mount",
			log.String("device_id", deviceID),
		)
		if err := p.Unmount(deviceID); err != nil {
			p.logger.Warn("failed to unmount on disconnect",
				log.String("device_id", deviceID),
				log.Error(err),
			)
		}
	}

	// Evict cached credentials to prevent slow memory leak.
	p.mu.Lock()
	delete(p.lastBody, deviceID)
	p.mu.Unlock()
}

// mountIsReadOnly reports whether the device's existing mount is read-only.
// False for a mount kcd did not create or no longer tracks.
func (p *SftpPlugin) mountIsReadOnly(deviceID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mountReadOnly[deviceID]
}

// Unmount cleanly unmounts a previously mounted SFTP filesystem.
// It first attempts a graceful shutdown of the sshfs process (SIGTERM → wait → SIGKILL),
// then uses fusermount to ensure the mount point is released.
//
// Errors distinguish the three cases a client has to react to differently:
// not mounted at all, a mount that exists but could not be released, and a
// clean unmount. Tracked state is dropped only once the mount is actually
// released, so a failed unmount can be retried instead of leaving the daemon
// believing the device is unmounted while the FUSE mount is still live.
func (p *SftpPlugin) Unmount(deviceID string) error {
	// Resolved rather than read from the map directly: a mount made before a
	// daemon restart is absent from the cache but still live, and reporting
	// "not mounted" there would leave it holding an sshfs process with no way
	// to clear it.
	mountPoint := p.MountedPath(deviceID)
	if mountPoint == "" {
		// Nothing mounted. A leftover directory can still be sitting there
		// from a mount whose release already succeeded, so clear it and report
		// the state the caller asked for rather than an error.
		if leftover := p.mountPointFor(deviceID); p.removeLeftoverDir(deviceID, leftover) {
			p.logger.Info("removed leftover SFTP mount directory",
				log.String("device_id", deviceID),
				log.String("mount_point", leftover),
			)
		}
		return fmt.Errorf("not mounted: no SFTP mount for device %s", deviceID)
	}

	// The kernel is the source of truth. A mount that has already gone -- the
	// FUSE connection died, or it was released by hand -- has nothing to
	// release, and fusermount will fail on it forever. Treating that as an
	// error while keeping the cached state would wedge the device permanently
	// in "mounted", unable to mount or unmount again.
	if !mountExists(mountPoint) {
		p.logger.Info("SFTP mount already released, clearing stale state",
			log.String("device_id", deviceID),
			log.String("mount_point", mountPoint),
		)
		p.finishUnmount(deviceID, mountPoint)
		return nil
	}

	p.mu.RLock()
	pid := p.mountPIDs[deviceID]
	p.mu.RUnlock()

	p.logger.Info("unmounting SFTP share", log.String("mount_point", mountPoint))

	// Graceful shutdown: SIGTERM → wait → SIGKILL. A mount adopted from the
	// kernel has no tracked PID, so look one up rather than skipping the
	// graceful step.
	hasPID := pid != 0
	if !hasPID {
		if found, err := findSSHFSPID(mountPoint); err == nil {
			pid, hasPID = found, true
		}
	}
	if hasPID {
		p.logger.Debug("sending SIGTERM to sshfs", log.Int("pid", pid))
		proc, err := os.FindProcess(pid)
		if err == nil {
			if err := proc.Signal(syscall.SIGTERM); err == nil {
				done := make(chan struct{})
				go func() {
					proc.Wait()
					close(done)
				}()
				select {
				case <-done:
					p.logger.Debug("sshfs exited cleanly after SIGTERM")
				case <-time.After(3 * time.Second):
					p.logger.Debug("sshfs did not exit after SIGTERM, sending SIGKILL")
					proc.Kill()
				}
			}
		}
	}

	// Ensure the mount point is released (bounded: a wedged FUSE mount
	// must not hang Unmount forever).
	tool := "fusermount3"
	if _, err := exec.LookPath(tool); err != nil {
		tool = "fusermount"
	}
	unmountCtx, unmountCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer unmountCancel()
	out, err := plugin.RunCommandSync(unmountCtx, tool, "-u", mountPoint)
	if err == nil {
		p.finishUnmount(deviceID, mountPoint)
		return nil
	}

	detail := strings.TrimSpace(string(out))

	// It may have gone while we were killing the sshfs process.
	if !mountExists(mountPoint) {
		p.logger.Info("SFTP mount released during unmount",
			log.String("mount_point", mountPoint),
			log.String("output", detail),
		)
		p.finishUnmount(deviceID, mountPoint)
		return nil
	}

	// Still present but not responding: a dead FUSE connection refuses a
	// normal unmount ("Transport endpoint is not connected"). A lazy unmount
	// detaches it anyway, and any process still inside will see ENOTCONN
	// rather than hang, which is the state a crashed mount is in regardless.
	if strings.Contains(detail, "Transport endpoint is not connected") {
		lazyOut, lazyErr := plugin.RunCommandSync(unmountCtx, tool, "-uz", mountPoint)
		if lazyErr == nil {
			p.logger.Info("SFTP mount force-detached with a lazy unmount",
				log.String("mount_point", mountPoint),
			)
			p.finishUnmount(deviceID, mountPoint)
			return nil
		}
		p.logger.Warn("lazy unmount failed",
			log.String("mount_point", mountPoint),
			log.Error(lazyErr),
			log.String("output", strings.TrimSpace(string(lazyOut))),
		)
		detail = detail + " (lazy unmount also failed: " + strings.TrimSpace(string(lazyOut)) + ")"
	}

	// Keep the tracked state: the mount is genuinely still there, and the
	// caller needs a retry to clear it.
	p.logger.Warn("fusermount failed",
		log.String("mount_point", mountPoint),
		log.Error(err),
		log.String("output", detail),
	)
	if detail != "" {
		detail = ": " + detail
	}
	return fmt.Errorf("stale SFTP mount at %s could not be released, %s failed: %v%s",
		mountPoint, tool, err, detail)
}

// finishUnmount clears tracked state and removes the mount directory. Called
// once the mount is confirmed released.
func (p *SftpPlugin) finishUnmount(deviceID, mountPoint string) {
	p.mu.Lock()
	delete(p.mountPoints, deviceID)
	delete(p.mountPIDs, deviceID)
	delete(p.mountReadOnly, deviceID)
	p.mu.Unlock()

	_ = os.Remove(mountPoint)
	p.logger.Info("SFTP unmounted", log.String("mount_point", mountPoint))
	p.publishUnmounted(deviceID, mountPoint)
}

// removeLeftoverDir removes a mount directory left behind by a mount that is
// no longer in the kernel, reporting whether there was one.
func (p *SftpPlugin) removeLeftoverDir(deviceID, mountPoint string) bool {
	if _, err := os.Stat(mountPoint); err != nil {
		return false
	}
	p.mu.Lock()
	delete(p.mountPoints, deviceID)
	delete(p.mountPIDs, deviceID)
	delete(p.mountReadOnly, deviceID)
	p.mu.Unlock()
	// Best effort: a non-empty or busy directory just stays, and the error
	// above still tells the caller nothing is mounted.
	return os.Remove(mountPoint) == nil
}

// findSSHFSPID scans /proc to find the sshfs daemon PID for a given mount point.
// Uses /proc directly to avoid external dependencies (pgrep, etc.).
func findSSHFSPID(mountPoint string) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		// cmdline uses null bytes as separators; convert to string for matching.
		if strings.Contains(string(cmdline), mountPoint) && strings.Contains(string(cmdline), "sshfs") {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no sshfs process found for mount point %s", mountPoint)
}

// UnmountAll releases every mount the plugin is tracking, and returns once
// they are all released or ctx is done.
//
// Shutdown has to do this. OnDisconnect only fires when a connection drops, so
// a graceful stop used to leave every mount live -- and with the mount
// directory under $XDG_STATE_HOME in the no-session fallback, `uninstall.sh
// --purge`'s `rm -rf` would then descend into a live mount and delete the
// phone's files. Even with the runtime-dir default, leaving a mount behind on
// every restart is not a reasonable way to end.
//
// Tracked devices are the source for the list rather than the device registry:
// this is the plugin's own record of what it believes is mounted, including
// mounts adopted from the kernel earlier in the session.
//
// Unmounts run concurrently because each can take up to 13s (3s waiting for
// sshfs to exit, then a 10s fusermount bound) and a serial loop over several
// devices would overrun the unit's TimeoutStopSec. ctx bounds the whole thing
// instead, and anything still running when it expires is logged -- the caller
// cannot do better, because systemd will SIGKILL next.
func (p *SftpPlugin) UnmountAll(ctx context.Context) {
	p.mu.RLock()
	ids := make([]string, 0, len(p.mountPoints))
	for id := range p.mountPoints {
		ids = append(ids, id)
	}
	p.mu.RUnlock()

	if len(ids) == 0 {
		return
	}
	p.logger.Info("releasing SFTP mounts on shutdown", log.Int("count", len(ids)))

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(deviceID string) {
			defer wg.Done()
			if err := p.Unmount(deviceID); err != nil {
				p.logger.Warn("could not release SFTP mount on shutdown",
					log.String("device_id", deviceID),
					log.Error(err),
				)
			}
		}(id)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		p.logger.Warn("timed out releasing SFTP mounts; some may survive this shutdown",
			log.Error(ctx.Err()),
		)
	}
}
