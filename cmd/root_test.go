package cmd

import (
	"testing"
	"time"
)

func TestEnvTruthy(t *testing.T) {
	for val, want := range map[string]bool{
		"":        false,
		"0":       false,
		"false":   false,
		"FALSE":   false,
		" false ": false,
		"no":      false,
		"off":     false,
		"1":       true,
		"true":    true,
		"yes":     true,
	} {
		t.Setenv("ROOKET_TEST_TRUTHY", val)
		if got := envTruthy("ROOKET_TEST_TRUTHY"); got != want {
			t.Errorf("envTruthy(%q) = %v, want %v", val, got, want)
		}
	}
}

func TestFmtDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		1234 * time.Millisecond:                "1.2s",
		24140 * time.Millisecond:               "24.1s",
		192*time.Second + 440*time.Millisecond: "3m12.4s",
		time.Hour + time.Minute + time.Second:  "1h1m1s",
	} {
		if got := fmtDur(d); got != want {
			t.Errorf("fmtDur(%v) = %q, want %q", d, got, want)
		}
	}
}
