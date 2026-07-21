package cmd

import (
	"os"
	"testing"
	"time"
)

func TestResolveColor(t *testing.T) {
	// A regular file is not a terminal (unlike /dev/null, which is a char
	// device); use it to exercise the auto path's negative case.
	reg, err := os.CreateTemp(t.TempDir(), "notatty")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	if orig, ok := os.LookupEnv("NO_COLOR"); ok {
		defer os.Setenv("NO_COLOR", orig)
	} else {
		defer os.Unsetenv("NO_COLOR")
	}
	os.Unsetenv("NO_COLOR")

	for _, c := range []struct {
		mode string
		want bool
	}{
		{"always", true},
		{"never", false},
		{"auto", false}, // regular file → not a terminal
		{"", false},
	} {
		got, err := resolveColor(c.mode, reg)
		if err != nil || got != c.want {
			t.Errorf("resolveColor(%q) = (%v, %v), want (%v, nil)", c.mode, got, err, c.want)
		}
	}

	os.Setenv("NO_COLOR", "")
	if got, _ := resolveColor("auto", reg); got {
		t.Error("NO_COLOR present must disable auto")
	}
	if got, _ := resolveColor("always", reg); !got {
		t.Error("explicit always must override NO_COLOR")
	}
	os.Unsetenv("NO_COLOR")

	if _, err := resolveColor("bogus", reg); err == nil {
		t.Error("invalid --color value should error")
	}
}

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
