package main

import (
	"testing"

	"github.com/bethropolis/kcd/internal/plugins/connectivity"
)

func TestFormatConnectivity(t *testing.T) {
	cases := []struct {
		name string
		body connectivity.ConnectivityBody
		want []string
	}{
		{
			name: "empty",
			body: connectivity.ConnectivityBody{},
			want: []string{"No SIM or cellular data available"},
		},
		{
			name: "single sim detailed type",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"0": {NetworkType: "LTE", NetworkDetailedType: "LTE", SignalStrength: 3},
			}},
			want: []string{"SIM 0: LTE      ●●●○"},
		},
		{
			name: "falls back to network type",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"0": {NetworkType: "5G", SignalStrength: 4},
			}},
			want: []string{"SIM 0: 5G       ●●●●"},
		},
		{
			name: "clamps above range to a full bar",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"0": {NetworkType: "GSM", SignalStrength: 9},
			}},
			want: []string{"SIM 0: GSM      ●●●●"},
		},
		{
			name: "clamps below range reads as no service",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"0": {NetworkType: "GSM", SignalStrength: -2},
			}},
			want: []string{"SIM 0: No service"},
		},
		{
			name: "zero level is no service, not an empty bar",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"0": {NetworkType: "LTE", SignalStrength: 0},
			}},
			want: []string{"SIM 0: No service"},
		},
		{
			name: "unknown network type reads as cellular",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"0": {NetworkType: "unknown", SignalStrength: 2},
			}},
			want: []string{"SIM 0: Cellular ●●○○"},
		},
		{
			name: "empty network type reads as cellular",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"0": {SignalStrength: 1},
			}},
			want: []string{"SIM 0: Cellular ●○○○"},
		},
		{
			name: "dual sim primary first and labels stay aligned",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"1": {NetworkType: "EDGE", SignalStrength: 2},
				"0": {NetworkType: "LTE", SignalStrength: 4},
			}},
			want: []string{"SIM 0: LTE      ●●●●", "SIM 1: EDGE     ●●○○"},
		},
		{
			name: "sparse subscription ids are not renumbered",
			body: connectivity.ConnectivityBody{SignalStrengths: map[string]connectivity.SignalStrength{
				"3": {NetworkType: "NR", SignalStrength: 3},
				"7": {NetworkType: "LTE", SignalStrength: 1},
			}},
			want: []string{"SIM 3: NR       ●●●○", "SIM 7: LTE      ●○○○"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatConnectivity(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
