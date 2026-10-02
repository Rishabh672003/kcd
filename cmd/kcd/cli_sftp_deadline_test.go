package main

import (
	"testing"
	"time"
)

// The daemon waits credentials_timeout_secs for the phone; the client must
// outlast it or the socket deadline wins and blames the connection.
func TestSftpCredentialDeadline_ExceedsDaemonWait(t *testing.T) {
	tests := []struct {
		name     string
		seconds  int
		wantWait time.Duration
	}{
		{"unset falls back to the daemon default", 0, 20 * time.Second},
		{"configured value", 45, 45 * time.Second},
		{"one second", 1, time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sftpCredentialDeadline(tc.seconds)
			if got <= tc.wantWait {
				t.Errorf("sftpCredentialDeadline(%d) = %v, must exceed the daemon's %v", tc.seconds, got, tc.wantWait)
			}
			if got > time.Duration(1<<62) {
				t.Errorf("sftpCredentialDeadline(%d) = %v, looks like an overflow", tc.seconds, got)
			}
		})
	}
}

func TestSftpCredentialDeadline_NoOverflow(t *testing.T) {
	const huge = 1 << 40 // seconds
	if got := sftpCredentialDeadline(huge); got <= 0 {
		t.Errorf("sftpCredentialDeadline returned %v for an absurd value", got)
	}
}
