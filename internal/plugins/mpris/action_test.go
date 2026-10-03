package mpris

import "testing"

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
