package sftp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bethropolis/kcd/internal/config"
	"github.com/bethropolis/kcd/internal/log"
)

func TestInfo_PasswordIsOptIn(t *testing.T) {
	p := NewSftpPlugin(config.SFTPConfig{}, nil, log.NewTest(t))
	p.lastBody["dev1"] = SftpBody{IP: "192.168.1.42", User: "u0_a123", Password: "hunter2", Path: "/storage/emulated/0"}

	if got := p.Info("dev1", false).Password; got != "" {
		t.Errorf("password leaked with includePassword=false: %q", got)
	}

	// Absent rather than empty, so a masked response stays distinguishable
	// from a device whose SFTP server genuinely has no password.
	raw, err := json.Marshal(p.Info("dev1", false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "password") {
		t.Errorf("password key present in masked JSON: %s", raw)
	}

	if got := p.Info("dev1", true).Password; got != "hunter2" {
		t.Errorf("includePassword=true returned %q, want %q", got, "hunter2")
	}
}
