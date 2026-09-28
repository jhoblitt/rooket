package lockowner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A holder's record replaces a longer one an earlier holder left, even with
// the file's offset past it, and reads back as the clause naming the holder.
func TestWriteThenAt(t *testing.T) {
	prev := os.Args
	os.Args = []string{"rooket", "up", "--name", "w6"}
	t.Cleanup(func() { os.Args = prev })

	path := filepath.Join(t.TempDir(), "w6.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("999999 rooket up --name " + strings.Repeat("x", 1000) + "\n"); err != nil {
		t.Fatal(err)
	}

	Write(f)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d rooket up --name w6\n", os.Getpid()); string(b) != want {
		t.Errorf("lock file holds %q, want %q", b, want)
	}
	if got, want := At(path), fmt.Sprintf(" (pid %d: rooket up --name w6)", os.Getpid()); got != want {
		t.Errorf("At = %q, want %q", got, want)
	}
}

func TestAtWithoutATrustworthyRecord(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		missing bool
		content string
	}{
		{name: "missing file", missing: true},
		{name: "empty file"},
		{name: "torn before the argv", content: "4321"},
		{name: "garbled pid", content: "not-a-pid rooket up\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".lock")
			if !tc.missing {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := At(path); got != "" {
				t.Errorf("At = %q, want empty", got)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	for _, tc := range []struct {
		content, want string
	}{
		{"4321 rooket up --workers 3\n", " (pid 4321: rooket up --workers 3)"},
		{"4321 rooket up\nleft over\n", " (pid 4321: rooket up)"},
		// Anything unexpected yields no attribution rather than a guess: the
		// read races the holder's own truncate-and-write.
		{"", ""},
		{"\n", ""},
		{"4321", ""},
		{"4321 ", ""},
		{"not-a-pid rooket up", ""},
		{"  ", ""},
	} {
		if got := format(tc.content); got != tc.want {
			t.Errorf("format(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}
