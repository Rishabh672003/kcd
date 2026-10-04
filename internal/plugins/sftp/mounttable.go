package sftp

import (
	"bufio"
	"os"
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

// mountExists reports whether the kernel currently has a FUSE mount at this
// exact path. Unmount uses it to tell "nothing is mounted" apart from "the
// release failed", which are very different outcomes for a caller.
func mountExists(path string) bool {
	_, found := eachMount(func(mountPoint string) bool { return mountPoint == path })
	return found
}

// eachMount returns the first FUSE mount for which match reports true, and
// whether any matched.
//
// FUSE-only on purpose: the daemon only ever creates `kcd-sftp-<deviceID>`
// mounts via sshfs, so adopting an unrelated mount that happens to share the
// name — and later running fusermount against it — would be worse than not
// adopting it.
func eachMount(match func(mountPoint string) bool) (string, bool) {
	f, err := os.Open(mountTablePath)
	if err != nil {
		return "", false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || !strings.HasPrefix(fields[2], "fuse") {
			continue
		}
		mountPoint := unescapeMountPath(fields[1])
		if match(mountPoint) {
			return mountPoint, true
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
