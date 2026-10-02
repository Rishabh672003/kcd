package sftp

import (
	"context"
	"fmt"
	"time"

	"github.com/bethropolis/kcd/internal/device"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
	"github.com/bethropolis/kcd/internal/protocol"
)

// RequestMount sends a kdeconnect.sftp.request packet asking the device to
// start its SFTP server and return credentials.
func (p *SftpPlugin) RequestMount(dev device.Sender) error {
	pkt, err := protocol.NewPacket("kdeconnect.sftp.request", map[string]any{
		"startBrowsing": true,
	})
	if err != nil {
		return err
	}
	return dev.Send(pkt)
}

// RequestAndMount sends the SFTP request, waits for the Android device to
// respond with credentials (up to 20 s), mounts the filesystem via sshfs,
// and returns the local path the user should open.
func (p *SftpPlugin) RequestAndMount(ctx context.Context, dev device.Sender) (string, error) {
	if p.bus == nil {
		return "", fmt.Errorf("event bus not available")
	}

	// Subscribe BEFORE sending the request to guarantee we don't miss the response.
	sub := p.bus.Subscribe(0, events.TypeSftpMount)
	defer sub.Close()

	if err := p.RequestMount(dev); err != nil {
		return "", fmt.Errorf("send SFTP request: %w", err)
	}

	p.logger.Info("SFTP request sent, waiting for phone response", log.String("device", dev.ID()))

	timeout := time.Duration(p.cfg.CredentialsTimeoutSecs) * time.Second
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case evt, ok := <-sub.C:
			if !ok {
				return "", fmt.Errorf("event bus closed")
			}
			if evt.DeviceID != dev.ID() {
				continue
			}
			p.mu.RLock()
			body, exists := p.lastBody[dev.ID()]
			p.mu.RUnlock()
			if !exists {
				return "", fmt.Errorf("credentials missing after event (internal error)")
			}
			return p.mountWithBody(ctx, dev.ID(), body, "")

		case <-deadline.Done():
			return "", fmt.Errorf("timed out after %s waiting for SFTP response — is the KDE Connect app open on the phone?", timeout)
		}
	}
}

// RequestAndMountVolume sends the SFTP request, waits for credentials, then
// mounts the specified volume. If volumePath is empty, the available volumes
// are returned without mounting (list mode). The caller is responsible for
// closing the returned closer when done with the mounted path.
func (p *SftpPlugin) RequestAndMountVolume(ctx context.Context, dev device.Sender, volumePath string) (mountPath string, volumes []StorageVolume, err error) {
	if p.bus == nil {
		return "", nil, fmt.Errorf("event bus not available")
	}

	sub := p.bus.Subscribe(0, events.TypeSftpMount)
	defer sub.Close()

	if err := p.RequestMount(dev); err != nil {
		return "", nil, fmt.Errorf("send SFTP request: %w", err)
	}

	p.logger.Info("SFTP request sent, waiting for phone response", log.String("device", dev.ID()))

	timeout := time.Duration(p.cfg.CredentialsTimeoutSecs) * time.Second
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case evt, ok := <-sub.C:
			if !ok {
				return "", nil, fmt.Errorf("event bus closed")
			}
			if evt.DeviceID != dev.ID() {
				continue
			}
			p.mu.RLock()
			body, exists := p.lastBody[dev.ID()]
			p.mu.RUnlock()
			if !exists {
				return "", nil, fmt.Errorf("credentials missing after event (internal error)")
			}

			vols := p.buildVolumes(body)

			if volumePath == "" {
				return "", vols, nil
			}

			path, err := p.mountWithBody(ctx, dev.ID(), body, volumePath)
			if err != nil {
				return "", nil, err
			}
			return path, vols, nil

		case <-deadline.Done():
			return "", nil, fmt.Errorf("timed out after %s waiting for SFTP response — is the KDE Connect app open on the phone?", timeout)
		}
	}
}

// MountLocally mounts using previously cached credentials.
// Prefer RequestAndMount for a one-step experience.
func (p *SftpPlugin) MountLocally(ctx context.Context, deviceID string) (string, error) {
	p.mu.RLock()
	body, ok := p.lastBody[deviceID]
	p.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("no SFTP credentials cached for device %s — use 'kcd sftp mount' which requests them automatically", deviceID)
	}
	return p.mountWithBody(ctx, deviceID, body, "")
}

// Info returns the cached SFTP connection details for a device.
// Returns nil if no credentials have been received yet.
//
// includePassword gates the credential: it is a working password for the
// phone's SFTP server, and `kcd sftp info` is an informational command whose
// output routinely lands in scrollback, logs and $(...) captures. Callers that
// genuinely need it (mounting) get credentials from the sftp.mount event
// instead, which is unaffected by this gate.
//
// Returns nil if no credentials have been received yet, even for a device that
// is still mounted -- mount state outlives the credential cache, which is
// cleared on disconnect.
func (p *SftpPlugin) Info(deviceID string, includePassword bool) *SftpInfo {
	p.mu.RLock()
	body, ok := p.lastBody[deviceID]
	var volumes []StorageVolume
	if ok {
		volumes = p.buildVolumes(body)
	}
	p.mu.RUnlock()

	if !ok {
		return nil
	}
	mountPoint := p.MountedPath(deviceID)
	info := &SftpInfo{
		IP:         body.IP,
		Port:       body.Port,
		User:       body.User,
		Path:       body.Path,
		Volumes:    volumes,
		Mounted:    mountPoint != "",
		MountPoint: mountPoint,
	}
	if includePassword {
		info.Password = body.Password
	}
	return info
}

// buildVolumes constructs a StorageVolume slice from a SftpBody.
// Caller must hold at least a read lock on p.mu if body comes from p.lastBody.
func (p *SftpPlugin) buildVolumes(body SftpBody) []StorageVolume {
	if len(body.MultiPaths) == 0 {
		return nil
	}
	volumes := make([]StorageVolume, 0, len(body.MultiPaths))
	for i, mp := range body.MultiPaths {
		name := mp
		if i < len(body.PathNames) {
			name = body.PathNames[i]
		}
		volumes = append(volumes, StorageVolume{Name: name, Path: mp})
	}
	return volumes
}

// Volumes returns the list of available storage volumes from cached credentials.
// Returns nil if no credentials or no multiPaths data.
func (p *SftpPlugin) Volumes(deviceID string) []StorageVolume {
	p.mu.RLock()
	defer p.mu.RUnlock()
	body, ok := p.lastBody[deviceID]
	if !ok {
		return nil
	}
	return p.buildVolumes(body)
}

// MountedPath returns the local mount point for a device, or "" if not mounted.
//
// Reconciles against the kernel mount table on a cache miss, so a mount made
// before a daemon restart is still reported as mounted.
func (p *SftpPlugin) MountedPath(deviceID string) string {
	p.mu.RLock()
	mountPoint, cached := p.mountPoints[deviceID]
	p.mu.RUnlock()
	if cached {
		return mountPoint
	}

	// The kernel is the source of truth. The configured location first, then
	// the location mounts used to default to, so a mount made before the
	// default moved is still found rather than orphaned.
	if mountPoint = p.adoptIfMounted(deviceID, p.mountPointFor(deviceID)); mountPoint != "" {
		return mountPoint
	}
	if legacy := legacyMountPointFor(deviceID); legacy != "" && legacy != p.mountPointFor(deviceID) {
		if mountPoint = p.adoptIfMounted(deviceID, legacy); mountPoint != "" {
			p.logger.Info("adopted SFTP mount left at the previous default location",
				log.String("device_id", deviceID),
				log.String("mount_point", mountPoint),
			)
			return mountPoint
		}
	}
	return ""
}

// adoptIfMounted caches mountPoint as this device's mount when the kernel
// reports it, and returns it; otherwise returns "".
func (p *SftpPlugin) adoptIfMounted(deviceID, mountPoint string) string {
	if !mountExists(mountPoint) {
		return ""
	}
	p.mu.Lock()
	p.mountPoints[deviceID] = mountPoint
	p.mu.Unlock()
	p.logger.Debug("adopted SFTP mount left by a previous daemon session",
		log.String("device_id", deviceID),
		log.String("mount_point", mountPoint),
	)
	return mountPoint
}

// IsMounted reports whether a device's filesystem is currently mounted.
func (p *SftpPlugin) IsMounted(deviceID string) bool {
	return p.MountedPath(deviceID) != ""
}
