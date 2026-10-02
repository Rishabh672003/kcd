package sftp

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/bethropolis/kcd/internal/log"
)

// userDirsFile is the freedesktop file naming the user's document folders.
// Reading it matters because those folders are relocatable: on a localized
// desktop Downloads may be "Descargas", and a check hardcoded to $HOME would
// miss exactly the setups where a user has customised them.
var userDirsFile = func() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "user-dirs.dirs")
}()

// bulkDeletableDirs are the XDG user folders whose contents a user is likely
// to wipe in one go, with the conventional name to fall back on when
// user-dirs.dirs has no entry.
//
// The category, not a single folder: the hazard is never "~/Documents", it is
// "anywhere a user runs rm -rf to reclaim space". Music and Pictures get wiped
// for exactly the same reason Downloads does -- someone clearing old photos is
// as plausible as someone clearing downloads -- so a narrower list would be
// arbitrary. This is the freedesktop set of user-content folders.
var bulkDeletableDirs = []struct{ key, name string }{
	{"XDG_DOWNLOAD_DIR", "Downloads"},
	{"XDG_DOCUMENTS_DIR", "Documents"},
	{"XDG_DESKTOP_DIR", "Desktop"},
	{"XDG_MUSIC_DIR", "Music"},
	{"XDG_PICTURES_DIR", "Pictures"},
	{"XDG_VIDEOS_DIR", "Videos"},
}

// userBulkDeletableDirs returns the resolved paths of the folders above.
func userBulkDeletableDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	configured := map[string]string{}
	if userDirsFile != "" {
		if f, openErr := os.Open(userDirsFile); openErr == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				if key, value, ok := parseUserDirsLine(scanner.Text()); ok {
					configured[key] = value
				}
			}
		}
	}

	out := make([]string, 0, len(bulkDeletableDirs))
	for _, d := range bulkDeletableDirs {
		dir, ok := configured[d.key]
		if !ok || dir == "" {
			dir = filepath.Join(home, d.name)
		}
		out = append(out, filepath.Clean(dir))
	}
	return out
}

// parseUserDirsLine reads one `KEY=value` line, unquoting the value and
// expanding $HOME. Values are quoted in the file because they may contain
// spaces, and the placeholder is literal there.
func parseUserDirsLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	key, value, found := strings.Cut(line, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
		value = value[1 : len(value)-1]
	}
	if value == "" {
		return "", "", false
	}
	if home, err := os.UserHomeDir(); err == nil {
		value = strings.ReplaceAll(value, "$HOME", home)
	}
	return key, value, true
}

// containingBulkDeletableDir returns the bulk-deletable folder path sits
// inside, or "" if it is not inside one.
func containingBulkDeletableDir(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	for _, dir := range userBulkDeletableDirs() {
		if isWithin(dir, abs) {
			return dir
		}
	}
	return ""
}

// isWithin reports whether path is dir itself or below it.
func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	// A leading ".." means path is outside dir; "." means it is dir itself.
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// warnIfInBulkDeletableDir emits the mount-location warning at most once per
// mount directory per daemon run, so a user who deliberately pinned a location
// is told once rather than on every mount.
func (p *SftpPlugin) warnIfInBulkDeletableDir(mountPoint string) {
	owner := containingBulkDeletableDir(mountPoint)

	p.mu.Lock()
	if p.warnedDirs == nil {
		p.warnedDirs = make(map[string]bool)
	}
	already := p.warnedDirs[mountPoint]
	p.warnedDirs[mountPoint] = true
	p.mu.Unlock()

	if already || owner == "" {
		return
	}
	p.logger.Warn("mount directory is inside a user folder that is often wiped in bulk. A mount makes the phone's storage reachable through the filesystem, and `rm -rf` descends into it -- rm only stops at a filesystem boundary when given -x/--one-file-system. Set mount_dir in kcd.toml to somewhere else, e.g. $XDG_RUNTIME_DIR/kcd/mnt.",
		log.String("mount_point", mountPoint),
		log.String("user_folder", owner),
	)
}
