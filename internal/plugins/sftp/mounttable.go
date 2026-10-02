package sftp

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// mountTablePath is the kernel's view of what is actually mounted. /proc is
// read directly rather than shelling out to findmnt, matching findSSHFSPID, so
// there is no external dependency and no PATH lookup to fail.
//
// It is a var so tests can point it at a fixture.
var mountTablePath = "/proc/mounts"

// liveMountPoint reports the mount point the kernel still has for a device, or
// ("", false) if there is none.
//
// This exists because mountPoints is in-memory. After a daemon restart the
// FUSE mount is still live, but the daemon has forgotten it: unmount would
// claim the device was never mounted while the mount sat there holding an
// sshfs process, and a later mount would try to run sshfs over it. Treating
// the kernel as the source of truth and the map as a cache makes that
// recoverable without persisting anything -- which is what we want, since a
// persisted entry would go stale after a crash and need reconciling anyway.
func liveMountPoint(deviceID string) (string, bool) {
	f, err := os.Open(mountTablePath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	// The daemon only ever creates mounts named after the device, so the
	// basename is already specific to us; the fstype check keeps an unrelated
	// mount that happens to share the name from being adopted.
	want := "kcd-sftp-" + deviceID
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		if !strings.HasPrefix(fields[2], "fuse") {
			continue
		}
		if filepath.Base(unescapeMountPath(fields[1])) == want {
			return unescapeMountPath(fields[1]), true
		}
	}
	return "", false
}

// unescapeMountPath reverses the octal escaping the kernel applies to a
// mountpoint containing whitespace ("/mnt/my\040disk" -> "/mnt/my disk").
// Getwd-style escapes are limited to space, tab, newline and backslash.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		var v byte
		switch {
		case s[i+1] >= '0' && s[i+1] <= '7' && s[i+2] >= '0' && s[i+2] <= '7' && s[i+3] >= '0' && s[i+3] <= '7':
			v = (s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0')
		default:
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(v)
		i += 3
	}
	return b.String()
}
