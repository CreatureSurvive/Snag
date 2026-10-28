package server

import (
	"testing"
	"time"
)

func TestFormatUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		1212*time.Hour + 59*time.Minute: "50 days, 12 hr",
		25 * time.Hour:                  "1 day, 1 hr",
		3*time.Hour + 5*time.Minute:     "3 hr, 5 min",
		42*time.Minute + 10*time.Second: "42 min",
	} {
		if got := formatUptime(d); got != want {
			t.Errorf("formatUptime(%v) = %q, want %q", d, got, want)
		}
	}
}
