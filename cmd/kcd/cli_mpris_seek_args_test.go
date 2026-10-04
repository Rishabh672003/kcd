package main

import (
	"errors"
	"testing"
)

// Issue #50: `kcd mpris seek -10s` was rejected by urfave/cli as an unknown
// flag. The seek command now skips flag parsing, so these cases must survive
// to parseSeek intact.
func TestSplitSeekArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    seekArgs
		wantErr bool
	}{
		{
			name: "negative offset",
			args: []string{"-10s"},
			want: seekArgs{offset: "-10s"},
		},
		{
			name: "negative offset after a flag",
			args: []string{"--device", "abc123", "-10s"},
			want: seekArgs{device: "abc123", offset: "-10s"},
		},
		{
			name: "negative offset with short player flag",
			args: []string{"-p", "Spotify", "-10s"},
			want: seekArgs{player: "Spotify", offset: "-10s"},
		},
		{
			name: "positive offset",
			args: []string{"+30s"},
			want: seekArgs{offset: "+30s"},
		},
		{
			name: "bare seconds",
			args: []string{"45"},
			want: seekArgs{offset: "45"},
		},
		{
			name: "equals form",
			args: []string{"--device=abc123", "--player=Spotify", "-10s"},
			want: seekArgs{device: "abc123", player: "Spotify", offset: "-10s"},
		},
		{
			name: "double dash workaround still works",
			args: []string{"--", "-10s"},
			want: seekArgs{offset: "-10s"},
		},
		{
			name:    "unknown flag is an error, not an offset",
			args:    []string{"--volume", "5", "-10s"},
			wantErr: true,
		},
		{
			name:    "flag missing its value is an error",
			args:    []string{"--device"},
			wantErr: true,
		},
		{
			name:    "missing offset is an error",
			args:    []string{"--device", "abc123"},
			wantErr: true,
		},
		{
			name:    "no args at all is an error",
			args:    nil,
			wantErr: true,
		},
		{
			// SkipFlagParsing means urfave/cli no longer handles -h itself.
			name:    "help flag is reported, not treated as an offset",
			args:    []string{"--help"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitSeekArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("splitSeekArgs(%q) = %+v, want error", tt.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitSeekArgs(%q) error: %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("splitSeekArgs(%q) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// SkipFlagParsing stops urfave/cli handling -h, so the command has to recognise
// it itself and show the subcommand help rather than reporting an unknown flag.
func TestSplitSeekArgsReportsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		_, err := splitSeekArgs([]string{arg})
		if !errors.Is(err, errSeekHelp) {
			t.Errorf("splitSeekArgs(%q) error = %v, want errSeekHelp", arg, err)
		}
	}
}

// The reported bug specifically: a negative offset must survive both arg
// splitting and duration parsing, and land as negative milliseconds.
func TestSplitSeekArgsNegativeParsesToNegativeMillis(t *testing.T) {
	args, err := splitSeekArgs([]string{"-10s"})
	if err != nil {
		t.Fatalf("splitSeekArgs: %v", err)
	}
	ms, err := parseSeek(args.offset)
	if err != nil {
		t.Fatalf("parseSeek(%q): %v", args.offset, err)
	}
	if ms != -10_000 {
		t.Errorf("parseSeek(%q) = %d, want -10000", args.offset, ms)
	}
}
