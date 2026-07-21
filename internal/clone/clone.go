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

// Dir is the .rooket directory inside a rook clone: the per-checkout sticky
// layer of values, profile selection, and ad-hoc templates.
type Dir struct{ root string }

func Open(rookDir string) Dir { return Dir{root: filepath.Join(rookDir, ".rooket")} }

func (d Dir) Path() string { return d.root }

// Ensure creates the directory tree and a .gitignore of "*". Git suppresses a
// directory whose every path is ignored, including the ignore file itself, so
// the rook checkout stays clean without touching .git/info/exclude or the
// tracked .gitignore.
func (d Dir) Ensure() error {
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
	return filepath.Join(d.root, "values", chart+".yaml")
}

func (d Dir) configPath() string { return filepath.Join(d.root, "config.yaml") }

type config struct {
	Profiles []string `yaml:"profiles"`
}

func (d Dir) Profiles() ([]string, error) {
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

func (d Dir) SetProfiles(names []string) error {
	if err := d.Ensure(); err != nil {
		return err
	}
	data, err := yaml.Marshal(config{Profiles: names})
	if err != nil {
		return fmt.Errorf("encode %s: %w", d.configPath(), err)
	}
	if err := os.WriteFile(d.configPath(), data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", d.configPath(), err)
	}
	return nil
}

func (d Dir) Templates() (map[string][]byte, error) {
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
