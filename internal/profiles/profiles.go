package profiles

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:embed all:builtin
var builtinFS embed.FS

// Reserved is the prefix rooket gives the clone's own templates in the
// generated chart, so no profile may claim it.
const Reserved = "local"

type Profile struct {
	Name        string
	Description string
	BuiltIn     bool
	Values      map[string]map[string]any
	Templates   map[string][]byte
}

type meta struct {
	Description string `yaml:"description"`
}

// Load returns a profile, preferring a user profile over a built-in of the same
// name; a user profile shadows a built-in entirely rather than merging with it.
func Load(userDir, name string) (Profile, error) {
	if name == Reserved {
		return Profile{}, fmt.Errorf("profile name %q is reserved for the clone's own templates", Reserved)
	}
	if dir := filepath.Join(userDir, name); isDir(dir) {
		return fromFS(os.DirFS(dir), name, false)
	}
	sub, err := fs.Sub(builtinFS, path.Join("builtin", name))
	if err == nil && fsHasFile(sub, "profile.yaml") {
		return fromFS(sub, name, true)
	}
	avail, _ := List(userDir)
	names := make([]string, 0, len(avail))
	for _, p := range avail {
		names = append(names, p.Name)
	}
	return Profile{}, fmt.Errorf("unknown profile %q (available: %s)", name, strings.Join(names, ", "))
}

func List(userDir string) ([]Profile, error) {
	byName := map[string]Profile{}

	builtins, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("read embedded profiles: %w", err)
	}
	for _, e := range builtins {
		if !e.IsDir() {
			continue
		}
		p, err := Load(userDir, e.Name())
		if err != nil {
			return nil, err
		}
		byName[e.Name()] = p
	}

	entries, err := os.ReadDir(userDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", userDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == Reserved {
			continue
		}
		p, err := Load(userDir, e.Name())
		if err != nil {
			return nil, err
		}
		byName[e.Name()] = p
	}

	out := make([]Profile, 0, len(byName))
	for _, p := range byName {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Fork copies a built-in profile into the user directory so it can be edited.
func Fork(userDir, name string) (string, error) {
	p, err := Load(userDir, name)
	if err != nil {
		return "", err
	}
	if !p.BuiltIn {
		return "", fmt.Errorf("profile %q is already a user profile at %s", name, filepath.Join(userDir, name))
	}
	dst := filepath.Join(userDir, name)
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("%s already exists", dst)
	}
	src, err := fs.Sub(builtinFS, path.Join("builtin", name))
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(src, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, rel)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return "", fmt.Errorf("fork profile %q: %w", name, err)
	}
	return dst, nil
}

func fromFS(fsys fs.FS, name string, builtIn bool) (Profile, error) {
	p := Profile{
		Name:      name,
		BuiltIn:   builtIn,
		Values:    map[string]map[string]any{},
		Templates: map[string][]byte{},
	}

	data, err := fs.ReadFile(fsys, "profile.yaml")
	if err != nil {
		return Profile{}, fmt.Errorf("profile %q: read profile.yaml: %w", name, err)
	}
	var m meta
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Profile{}, fmt.Errorf("profile %q: parse profile.yaml: %w", name, err)
	}
	p.Description = m.Description

	valueFiles, err := fs.ReadDir(fsys, "values")
	if err == nil {
		for _, e := range valueFiles {
			if e.IsDir() || !isYAML(e.Name()) {
				continue
			}
			raw, err := fs.ReadFile(fsys, path.Join("values", e.Name()))
			if err != nil {
				return Profile{}, fmt.Errorf("profile %q: read %s: %w", name, e.Name(), err)
			}
			var v map[string]any
			if err := yaml.Unmarshal(raw, &v); err != nil {
				return Profile{}, fmt.Errorf("profile %q: parse %s: %w", name, e.Name(), err)
			}
			p.Values[strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".yaml"), ".yml")] = v
		}
	}

	tmplFiles, err := fs.ReadDir(fsys, "templates")
	if err == nil {
		for _, e := range tmplFiles {
			if e.IsDir() || !isYAML(e.Name()) {
				continue
			}
			raw, err := fs.ReadFile(fsys, path.Join("templates", e.Name()))
			if err != nil {
				return Profile{}, fmt.Errorf("profile %q: read %s: %w", name, e.Name(), err)
			}
			p.Templates[e.Name()] = raw
		}
	}
	return p, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func fsHasFile(fsys fs.FS, name string) bool {
	_, err := fs.Stat(fsys, name)
	return err == nil
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}
