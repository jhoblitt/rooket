package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDeviceLink(t *testing.T) {
	dir := t.TempDir()

	t.Run("absolute target", func(t *testing.T) {
		link := filepath.Join(dir, "abs")
		if err := os.Symlink("/dev/sdz", link); err != nil {
			t.Fatal(err)
		}
		if got := resolveDeviceLink(link); got != "/dev/sdz" {
			t.Errorf("resolveDeviceLink = %q, want /dev/sdz", got)
		}
	})

	t.Run("relative target resolved against the link's directory", func(t *testing.T) {
		link := filepath.Join(dir, "rel")
		if err := os.Symlink("../../dev/sdy", link); err != nil {
			t.Fatal(err)
		}
		want := filepath.Clean(filepath.Join(dir, "../../dev/sdy"))
		if got := resolveDeviceLink(link); got != want {
			t.Errorf("resolveDeviceLink = %q, want %q", got, want)
		}
	})

	t.Run("missing link", func(t *testing.T) {
		if got := resolveDeviceLink(filepath.Join(dir, "nope")); got != "" {
			t.Errorf("resolveDeviceLink on a missing link = %q, want empty", got)
		}
	})
}
