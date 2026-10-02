package sftp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bethropolis/kcd/internal/config"
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

func TestUnmount_KeepsStateWhenFusermountFails(t *testing.T) {
	dir := t.TempDir()
	p := newTestPlugin(t, dir)

	mountPoint := filepath.Join(dir, "kcd-sftp-dev1")
	if err := os.MkdirAll(mountPoint, 0700); err != nil {
		t.Fatal(err)
	}
	p.mountPoints["dev1"] = mountPoint

	// No sshfs process is running for this path, so fusermount has nothing to
	// release and fails. Tracked state must survive so the caller can retry.
	err := p.Unmount("dev1")
	if err == nil {
		t.Skip("fusermount succeeded on a path that was never mounted; cannot exercise the failure path")
	}
	if !strings.Contains(err.Error(), "could not be released") {
		t.Errorf("error should name the stale mount and the failing tool, got: %v", err)
	}
	if _, stillTracked := p.mountPoints["dev1"]; !stillTracked {
		t.Error("mount state was dropped even though the unmount failed; the daemon would claim unmounted while the FUSE mount is live")
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
