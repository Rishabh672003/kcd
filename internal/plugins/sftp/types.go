package sftp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bethropolis/kcd/internal/config"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
)

// SftpPlugin handles KDE Connect SFTP negotiation and optional sshfs mounting.
type SftpPlugin struct {
	cfg         config.SFTPConfig
	bus         *events.Bus
	logger      log.Logger
	mu          sync.RWMutex
	lastBody    map[string]SftpBody
	mountPoints map[string]string // deviceID -> local mountPoint path
	mountPIDs   map[string]int    // deviceID -> sshfs PID for graceful shutdown
}

func NewSftpPlugin(cfg config.SFTPConfig, bus *events.Bus, logger log.Logger) *SftpPlugin {
	return &SftpPlugin{
		cfg:         cfg,
		bus:         bus,
		logger:      logger.With(log.String("plugin", "sftp")),
		lastBody:    make(map[string]SftpBody),
		mountPoints: make(map[string]string),
		mountPIDs:   make(map[string]int),
	}
}

// SftpBody matches the body of a kdeconnect.sftp packet sent by the Android app.
type SftpBody struct {
	IP   string      `json:"ip"`
	Port json.Number `json:"port"`
	User string      `json:"user"`
	// Password is intentionally not logged.
	Password string `json:"password"`
	// Path is the primary storage root path from the Android device.
	// When exactly one volume exists this is the volume path (e.g. /storage/emulated/0);
	// when multiple volumes exist this falls back to "/" (legacy compat).
	// Prefer MultiPaths for the authoritative list of browsable roots.
	Path string `json:"path"`
	// MultiPaths lists all available storage root paths on the device
	// (e.g. internal storage, SD card). Populated by Android API 30+.
	MultiPaths []string `json:"multiPaths,omitempty"`
	// PathNames provides human-readable labels for each path in MultiPaths.
	PathNames []string `json:"pathNames,omitempty"`
	// ErrorMessage is set when the device cannot start the SFTP server
	// (e.g. missing storage permissions).
	ErrorMessage string `json:"errorMessage,omitempty"`
}

// StorageVolume describes a single browsable storage root on the device.
type StorageVolume struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SftpInfo holds the complete cached SFTP connection details for a device.
//
// Password is a live credential for the phone's SFTP server, so it is left
// empty unless the caller explicitly asked for it (see Info). omitempty keeps
// the field out of the JSON entirely rather than emitting an empty string,
// so a masked response cannot be mistaken for a device with no password.
type SftpInfo struct {
	IP       string          `json:"ip"`
	Port     json.Number     `json:"port"`
	User     string          `json:"user"`
	Password string          `json:"password,omitempty"`
	Path     string          `json:"path"`
	Volumes  []StorageVolume `json:"volumes,omitempty"`

	// Mounted reports whether this device's filesystem is currently mounted,
	// and MountPoint is where. Clients use it to render a mount toggle
	// without inspecting the host's mount table.
	Mounted    bool   `json:"mounted"`
	MountPoint string `json:"mountPoint,omitempty"`
}

func (p *SftpPlugin) Name() string            { return "SFTP" }
func (p *SftpPlugin) Timeout() time.Duration  { return 5 * time.Second }
func (p *SftpPlugin) IncomingTypes() []string { return []string{"kdeconnect.sftp"} }
func (p *SftpPlugin) OutgoingTypes() []string { return []string{"kdeconnect.sftp.request"} }

// mountPointFor is the one mount path the daemon creates for a device.
// Derived rather than looked up, so it still identifies leftovers when the
// cache is empty -- which is exactly the state after a restart.
//
// An empty MountDir falls back to the same default config.Defaults() uses,
// rather than to a temp directory: the daemon never writes the config file,
// so `mount_dir = ""` is an explicit request for the default, not a way to
// ask for somewhere that gets swept up by periodic tmp cleaning.
func (p *SftpPlugin) mountPointFor(deviceID string) string {
	baseDir := p.cfg.MountDir
	if baseDir == "" {
		baseDir = config.DefaultMountDir()
	}
	return filepath.Join(baseDir, "kcd-sftp-"+deviceID)
}

// legacyMountPointFor is where mounts lived before the default moved out of
// ~/Downloads. Mounts there outlive the daemon, so Unmount has to keep being
// able to find and release them.
func legacyMountPointFor(deviceID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Downloads", "kcd", "mnt", "kcd-sftp-"+deviceID)
}
