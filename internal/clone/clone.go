package clone

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Dir is a configuration home: the .rooket directory inside a rook clone, or
// a directory the user named with --config-dir. The zero Dir is no
// configuration at all.
type Dir struct {
	root string
	// named marks a directory the user chose. It is meant to be committed, so
	// rooket never gives it the self-ignoring .gitignore a clone's .rooket gets.
	named bool
}

func Open(rookDir string) Dir { return Dir{root: filepath.Join(rookDir, ".rooket")} }

// At opens a configuration directory the user named.
func At(dir string) Dir { return Dir{root: dir, named: true} }

func (d Dir) Path() string { return d.root }

// Ensure creates the directory tree and a .gitignore of "*", but only for a
// clone's own .rooket. Git suppresses a directory whose every path is
// ignored, including the ignore file itself, so the rook checkout stays
// clean without touching .git/info/exclude or the tracked .gitignore. A
// named directory or the zero Dir is left untouched: the user means to
// commit a named directory, and there is nothing to create for no
// configuration home at all.
func (d Dir) Ensure() error {
	if d.named || d.root == "" {
		return nil
	}
	for _, sub := range []string{"values", "templates"} {
		if err := os.MkdirAll(filepath.Join(d.root, sub), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d.root, err)
		}
	}
	gi := filepath.Join(d.root, ".gitignore")
	if _, err := os.Stat(gi); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(gi, []byte("*\n"), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", gi, err)
		}
	}
	return nil
}

func (d Dir) ValuesPath(chart string) string {
	if d.root == "" {
		return ""
	}
	return filepath.Join(d.root, "values", chart+".yaml")
}

func (d Dir) configPath() string { return filepath.Join(d.root, "config.yaml") }

type config struct {
	Profiles []string `yaml:"profiles"`
}

func (d Dir) Profiles() ([]string, error) {
	if d.root == "" {
		return nil, nil
	}
	data, err := os.ReadFile(d.configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", d.configPath(), err)
	}
	var c config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", d.configPath(), err)
	}
	return c.Profiles, nil
}

func (d Dir) Templates() (map[string][]byte, error) {
	if d.root == "" {
		return nil, nil
	}
	dir := filepath.Join(d.root, "templates")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, e.Name()), err)
		}
		out[e.Name()] = data
	}
	return out, nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
