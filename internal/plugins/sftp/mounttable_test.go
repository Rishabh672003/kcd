package sftp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnescapeMountPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain path is untouched", in: "/mnt/kcd-sftp-dev1", want: "/mnt/kcd-sftp-dev1"},
		{name: "space", in: `/mnt/my\040disk`, want: "/mnt/my disk"},
		{name: "tab", in: `/mnt/a\011b`, want: "/mnt/a\tb"},
		{name: "newline", in: `/mnt/a\012b`, want: "/mnt/a\nb"},
		{name: "backslash", in: `/mnt/a\134b`, want: `/mnt/a\b`},
		{name: "several escapes", in: `/mnt/a\040b\040c`, want: "/mnt/a b c"},
		{name: "trailing lone backslash", in: `/mnt/odd\`, want: `/mnt/odd\`},
		{name: "non-octal escape is left alone", in: `/mnt/a\999b`, want: `/mnt/a\999b`},
		{name: "truncated escape is left alone", in: `/mnt/a\04`, want: `/mnt/a\04`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := unescapeMountPath(tc.in); got != tc.want {
				t.Errorf("unescapeMountPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLiveMountPoint_IgnoresUnrelatedMounts(t *testing.T) {
	// A device with no live mount must never be reported as mounted. The test
	// machine may or may not have any FUSE mounts, so the only safe assertion
	// is that an id we certainly never mounted comes back absent.
	if mp, ok := liveMountPoint("kcd-nonexistent-device-9a5c23ea7195"); ok {
		t.Errorf("invented device reported as mounted at %q", mp)
	}
}

// useFakeMountTable points the reconciliation at a fixture for one test.
func useFakeMountTable(t *testing.T, contents string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := mountTablePath
	mountTablePath = path
	t.Cleanup(func() { mountTablePath = orig })
}

// A device whose mount is absent from the cache but present in the kernel is
// exactly the post-restart state: the FUSE mount survived, the daemon's memory
// did not. It has to be adopted, or unmount reports "not mounted" while the
// mount is still holding an sshfs process.
func TestMountedPath_AdoptsOrphanFromKernel(t *testing.T) {
	useFakeMountTable(t, `sysfs /sys sysfs rw 0 0
proc /proc proc rw 0 0
phone:/storage/emulated/0 /home/user/Downloads/kcd/mnt/kcd-sftp-dev1 fuse.sshfs rw,nosuid,nodev 0 0
`)

	p := newTestPlugin(t, t.TempDir())
	if _, cached := p.mountPoints["dev1"]; cached {
		t.Fatal("test precondition: cache should start empty")
	}

	got := p.MountedPath("dev1")
	if got != "/home/user/Downloads/kcd/mnt/kcd-sftp-dev1" {
		t.Fatalf("MountedPath = %q, want the kernel's mount point", got)
	}
	if _, adopted := p.mountPoints["dev1"]; !adopted {
		t.Error("orphan was resolved but not adopted into the cache, so every lookup re-reads /proc")
	}
	if !p.IsMounted("dev1") {
		t.Error("IsMounted = false for a live mount the daemon had forgotten")
	}
}

// A different device's mount must not be adopted for this one.
func TestMountedPath_DoesNotAdoptAnotherDevice(t *testing.T) {
	useFakeMountTable(t, `phone:/storage/emulated/0 /mnt/kcd-sftp-other fuse.sshfs rw 0 0
`)

	p := newTestPlugin(t, t.TempDir())
	if got := p.MountedPath("dev1"); got != "" {
		t.Errorf("adopted another device's mount: %q", got)
	}
}

// Non-FUSE mounts sharing the naming scheme must be left alone: adopting one
// and later running fusermount against it would be worse than not adopting.
func TestMountedPath_IgnoresNonFuseEntries(t *testing.T) {
	useFakeMountTable(t, `/dev/sdb1 /mnt/kcd-sftp-dev1 ext4 rw 0 0
`)

	p := newTestPlugin(t, t.TempDir())
	if got := p.MountedPath("dev1"); got != "" {
		t.Errorf("adopted a non-FUSE mount: %q", got)
	}
}

// Mount points with spaces are octal-escaped by the kernel.
func TestMountedPath_UnescapesKernelPath(t *testing.T) {
	useFakeMountTable(t, `phone:/storage/ABCD /mnt/my\040disk/kcd-sftp-dev1 fuse.sshfs rw 0 0
`)

	p := newTestPlugin(t, t.TempDir())
	if got := p.MountedPath("dev1"); got != "/mnt/my disk/kcd-sftp-dev1" {
		t.Errorf("MountedPath = %q, want the unescaped path", got)
	}
}

// An absent mount reports not mounted and stays out of the cache.
func TestMountedPath_AbsentLeavesCacheClean(t *testing.T) {
	useFakeMountTable(t, "proc /proc proc rw 0 0\n")

	p := newTestPlugin(t, t.TempDir())
	if got := p.MountedPath("dev1"); got != "" {
		t.Errorf("MountedPath = %q, want empty", got)
	}
	if _, cached := p.mountPoints["dev1"]; cached {
		t.Error("cached an empty resolution")
	}
}
