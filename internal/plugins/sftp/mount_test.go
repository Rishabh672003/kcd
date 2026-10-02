package sftp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bethropolis/kcd/internal/config"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
)

func newTestPlugin(t *testing.T, mountDir string) *SftpPlugin {
	t.Helper()
	cfg := config.SFTPConfig{}
	cfg.MountDir = mountDir
	return NewSftpPlugin(cfg, nil, log.NewTest(t))
}

// Mounting an already-mounted device must not reach sshfs. Verified without a
// real sshfs by pointing an existing mount entry at the path: a second mount
// returns that path instead of trying to exec over it.
func TestMountWithBody_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := newTestPlugin(t, dir)

	existing := filepath.Join(dir, "kcd-sftp-dev1")
	if err := os.MkdirAll(existing, 0700); err != nil {
		t.Fatal(err)
	}
	p.mountPoints["dev1"] = existing

	body := SftpBody{IP: "192.168.1.42", Port: "1776", User: "u0_a123", Password: "x", Path: "/storage/emulated/0"}

	got, err := p.mountWithBody(context.Background(), "dev1", body, "")
	if err != nil {
		t.Fatalf("second mount should succeed by reusing the mount point, got: %v", err)
	}
	if got != existing {
		t.Errorf("second mount returned %q, want the existing mount point %q", got, existing)
	}
}

func TestUnmount_NotMountedIsDistinguishable(t *testing.T) {
	p := newTestPlugin(t, t.TempDir())

	err := p.Unmount("never-mounted")
	if err == nil {
		t.Fatal("expected an error for an unmounted device")
	}
	if !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("error should say 'not mounted' so clients can react to it, got: %v", err)
	}
}

func TestSshfsHint(t *testing.T) {
	tests := []struct {
		name    string
		msg     string
		wantSub string // "" means no hint expected
	}{
		{
			name:    "fuse permission denied",
			msg:     "fusermount3: failed to access mountpoint /mnt/x: Permission denied",
			wantSub: "/etc/fuse.conf",
		},
		{
			name:    "operation not permitted",
			msg:     "fusermount: mount failed: Operation not permitted",
			wantSub: "/etc/fuse.conf",
		},
		{
			name:    "sshfs binary missing",
			msg:     `exec: "sshfs": executable file not found in $PATH`,
			wantSub: "sshfs is not installed",
		},
		{
			// Textually indistinguishable from the fuse.conf case above, and
			// genuinely ambiguous on its own. mountWithBody is now idempotent,
			// so this message is no longer produced by a duplicate mount --
			// the ambiguity is resolved upstream rather than by the hint.
			name:    "permission denied on mountpoint",
			msg:     "fusermount3: failed to access mountpoint /mnt/kcd-sftp-dev1: Permission denied",
			wantSub: "/etc/fuse.conf",
		},
		{
			// Names fusermount3 but is not a fuse.conf problem: this is the
			// class of error the old substring match mis-advised on.
			name:    "unrelated fusermount error",
			msg:     "fusermount3: entry for /mnt/kcd-sftp-dev1 is not a directory",
			wantSub: "",
		},
		{
			name:    "unknown failure",
			msg:     "read: Connection reset by peer",
			wantSub: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sshfsHint(tc.msg)
			if tc.wantSub == "" {
				if got != "" {
					t.Errorf("expected no hint, got:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("hint missing %q, got:\n%s", tc.wantSub, got)
			}
		})
	}
}

// Mount state transitions must reach the bus, since that is the only way a
// client can render a mount toggle without inspecting the host mount table.
func TestMountStateEventsPublished(t *testing.T) {
	bus := events.NewBus(log.NewTest(t))
	sub := bus.Subscribe(0, events.TypeSftpMounted, events.TypeSftpUnmounted)
	defer sub.Close()

	p := newTestPlugin(t, t.TempDir())
	p.bus = bus

	// The idempotent path reuses the mount point without a state change, so it
	// must not announce a transition that did not happen.
	p.mountPoints["dev1"] = "/mnt/kcd-sftp-dev1"
	if _, err := p.mountWithBody(context.Background(), "dev1", SftpBody{}, ""); err != nil {
		t.Fatalf("idempotent mount: %v", err)
	}
	select {
	case ev := <-sub.C:
		t.Fatalf("unexpected event on a no-op mount: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}

	p.publishMounted("dev1", "/mnt/kcd-sftp-dev1", "/storage/ABCD-1234")
	ev := <-sub.C
	if ev.Type != events.TypeSftpMounted {
		t.Fatalf("got %s, want %s", ev.Type, events.TypeSftpMounted)
	}
	if got := ev.Payload.(map[string]any)["mountPoint"]; got != "/mnt/kcd-sftp-dev1" {
		t.Errorf("mountPoint = %v, want /mnt/kcd-sftp-dev1", got)
	}
	if got := ev.Payload.(map[string]any)["volume"]; got != "/storage/ABCD-1234" {
		t.Errorf("volume = %v, want /storage/ABCD-1234", got)
	}

	p.publishUnmounted("dev1", "/mnt/kcd-sftp-dev1")
	ev = <-sub.C
	if ev.Type != events.TypeSftpUnmounted {
		t.Fatalf("got %s, want %s", ev.Type, events.TypeSftpUnmounted)
	}
}

func TestInfoReportsMountState(t *testing.T) {
	p := newTestPlugin(t, t.TempDir())
	p.lastBody["dev1"] = SftpBody{IP: "192.168.1.42", User: "u0_a123", Path: "/storage/emulated/0"}

	if info := p.Info("dev1", false); info.Mounted {
		t.Error("reported mounted with no mount tracked")
	}

	p.mountPoints["dev1"] = "/mnt/kcd-sftp-dev1"
	info := p.Info("dev1", false)
	if !info.Mounted {
		t.Error("reported not mounted while a mount is tracked")
	}
	if info.MountPoint != "/mnt/kcd-sftp-dev1" {
		t.Errorf("MountPoint = %q, want /mnt/kcd-sftp-dev1", info.MountPoint)
	}
}

// The reported bug: the daemon still had a device cached as mounted, but the
// kernel had no such mount, so fusermount failed with "not found in
// /etc/mtab" forever. Keeping the cached state on failure -- correct for a
// mount that really is still there -- meant this state could never clear,
// leaving the device unable to mount or unmount again.
func TestUnmount_ClearsStateWhenKernelHasNoMount(t *testing.T) {
	dir := t.TempDir()
	useFakeMountTable(t, "proc /proc proc rw 0 0\n")

	p := newTestPlugin(t, dir)
	mountPoint := filepath.Join(dir, "kcd-sftp-dev1")
	if err := os.MkdirAll(mountPoint, 0700); err != nil {
		t.Fatal(err)
	}
	p.mountPoints["dev1"] = mountPoint

	if err := p.Unmount("dev1"); err != nil {
		t.Fatalf("unmounting a mount the kernel already dropped must succeed, got: %v", err)
	}
	if _, cached := p.mountPoints["dev1"]; cached {
		t.Error("cached state survived; the device stays wedged as mounted")
	}
	if _, err := os.Stat(mountPoint); !os.IsNotExist(err) {
		t.Error("leftover mount directory was not removed")
	}
	if p.MountedPath("dev1") != "" {
		t.Error("MountedPath still reports the device as mounted")
	}
}

// With an empty cache and nothing in the kernel, a leftover directory from an
// earlier release should still be cleaned up.
func TestUnmount_RemovesLeftoverDirectoryWithoutCache(t *testing.T) {
	dir := t.TempDir()
	useFakeMountTable(t, "proc /proc proc rw 0 0\n")

	p := newTestPlugin(t, dir)
	leftover := filepath.Join(dir, "kcd-sftp-dev1")
	if err := os.MkdirAll(leftover, 0700); err != nil {
		t.Fatal(err)
	}

	err := p.Unmount("dev1")
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Errorf("want a 'not mounted' error, got: %v", err)
	}
	if _, statErr := os.Stat(leftover); !os.IsNotExist(statErr) {
		t.Error("leftover directory survived; it will block the next mount's MkdirAll forever")
	}
}

// A genuinely live mount must still be released through fusermount, and the
// normal path must not be short-circuited by the new kernel check.
func TestUnmount_LiveMountTakesTheReleasePath(t *testing.T) {
	dir := t.TempDir()
	mountPoint := filepath.Join(dir, "kcd-sftp-dev1")
	if err := os.MkdirAll(mountPoint, 0700); err != nil {
		t.Fatal(err)
	}

	// Present in the kernel's view, so the release path runs. Nothing is
	// actually mounted here, so fusermount will fail -- the point is that we
	// got as far as trying it and then kept the state, because the mount is
	// (as far as we can tell) still there.
	useFakeMountTable(t, "phone:/storage/emulated/0 "+mountPoint+" fuse.sshfs rw 0 0\n")

	p := newTestPlugin(t, dir)
	p.mountPoints["dev1"] = mountPoint

	err := p.Unmount("dev1")
	if err != nil && !strings.Contains(err.Error(), "could not be released") {
		t.Errorf("unexpected error shape: %v", err)
	}
	if _, cached := p.mountPoints["dev1"]; !cached {
		t.Error("state was dropped for a mount the kernel still reports; it should stay retryable")
	}
}
