package mpris

import (
	"encoding/json"
	"testing"

	"github.com/bethropolis/kcd/internal/config"
	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/internal/log"
	"github.com/bethropolis/kcd/internal/protocol"
)

func TestCanonicalAction(t *testing.T) {
	for in, want := range map[string]string{
		"playpause": "PlayPause",
		"PlayPause": "PlayPause",
		"PLAY":      "Play",
		"pause":     "Pause",
		"next":      "Next",
		"previous":  "Previous",
		"stop":      "Stop",
		"raise":     "raise", // unknown: unchanged
		"":          "",
	} {
		if got := CanonicalAction(in); got != want {
			t.Errorf("CanonicalAction(%q) = %q, want %q", in, got, want)
		}
	}
}

type captureSender struct {
	testSender
	sent []*protocol.Packet
}

func (s *captureSender) Send(p *protocol.Packet) error { s.sent = append(s.sent, p); return nil }

func ptr[T any](v T) *T { return &v }

// A relative seek is sent as SetPosition.
func TestSendActionSendsSeekAsSetPosition(t *testing.T) {
	p := NewMPRISPlugin(nil, events.NewBus(log.Nop()), false, config.MPRISConfig{}, log.Nop())
	p.remoteStates["phone"] = &NowPlaying{Player: "music", Pos: 60_000, Length: 200_000}

	for _, tc := range []struct {
		name         string
		seek, setPos *int64
		wantPos      int64
	}{
		{"forward", ptr[int64](30_000), nil, 90_000},
		{"clamped at start", ptr[int64](-90_000), nil, 0},
		{"clamped at end", ptr[int64](500_000), nil, 200_000},
		{"absolute wins", ptr[int64](30_000), ptr[int64](5_000), 5_000},
	} {
		dev := &captureSender{testSender: testSender{id: "phone"}}
		if err := p.SendAction(dev, "music", "", tc.seek, tc.setPos, nil); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var body map[string]any
		if err := json.Unmarshal(dev.sent[0].Body, &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["Seek"]; ok {
			t.Errorf("%s: Seek sent alongside SetPosition: %v", tc.name, body)
		}
		if got, _ := body["SetPosition"].(float64); int64(got) != tc.wantPos {
			t.Errorf("%s: SetPosition = %v, want %d", tc.name, body["SetPosition"], tc.wantPos)
		}
	}
}
