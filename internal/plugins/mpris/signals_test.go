package mpris

import "testing"

func TestClassifySignalMatchesQualifiedNames(t *testing.T) {
	tests := []struct {
		name   string
		signal string
		want   signalKind
	}{
		{"NameOwnerChanged", "org.freedesktop.DBus.NameOwnerChanged", signalKindNameOwnerChanged},
		{"Seeked", "org.mpris.MediaPlayer2.Player.Seeked", signalKindSeeked},
		{"PropertiesChanged", "org.freedesktop.DBus.Properties.PropertiesChanged", signalKindPropertiesChanged},

		// godbus reports "<interface>.<member>". Matching the bare member
		// silently dropped every signal, so these must stay unrouted.
		{"bare NameOwnerChanged", "NameOwnerChanged", signalKindNone},
		{"bare Seeked", "Seeked", signalKindNone},
		{"bare PropertiesChanged", "PropertiesChanged", signalKindNone},

		{"other interface", "org.freedesktop.DBus.ObjectManager.InterfacesAdded", signalKindNone},
		{"player interface other member", "org.mpris.MediaPlayer2.Player.VolumeChanged", signalKindNone},
		{"empty", "", signalKindNone},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifySignal(tc.signal); got != tc.want {
				t.Errorf("classifySignal(%q) = %v, want %v", tc.signal, got, tc.want)
			}
		})
	}
}
