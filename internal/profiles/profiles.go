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
var embeddedBuiltinFS embed.FS

// builtinFS is an fs.FS (rather than the concrete embed.FS above) so tests in
// this package can substitute a broken filesystem to exercise error paths.
var builtinFS fs.FS = embeddedBuiltinFS

// Reserved is the prefix rooket gives the clone's own templates in the
// generated chart, so no profile may claim it.
const Reserved = "local"

type Profile struct {
	Name        string
	Description string
	BuiltIn     bool
	// Path is the directory exactly as the caller gave it, for a profile
	// loaded by LoadDir; empty for one loaded by name.
	Path      string
	Values    map[string]map[string]any
	Templates map[string][]byte

	// dir is Path made absolute, so two spellings of one directory compare
	// equal in SameSource.
	dir string
}

// Label is how the user selected the profile: its path for one loaded by
// LoadDir, else its name.
func (p Profile) Label() string {
	if p.Path != "" {
		return p.Path
	}
	return p.Name
}

// SameSource reports whether two profiles of the same name were loaded from
// the same place. Load always resolves a name to the same profile, so two
// loaded by name match; a path profile matches only the same directory.
func (p Profile) SameSource(q Profile) bool { return p.dir == q.dir }

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
	names, err := availableNames(userDir)
	if err != nil {
		return Profile{}, fmt.Errorf("unknown profile %q: %w", name, err)
	}
	return Profile{}, fmt.Errorf("unknown profile %q (available: %s)", name, strings.Join(names, ", "))
}

// LoadDir loads the profile in the directory at path, naming it for the
// directory's basename. A relative path resolves against the working
// directory.
func LoadDir(path string) (Profile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	if !fi.IsDir() {
		return Profile{}, fmt.Errorf("profile %s: not a directory", path)
	}
	name := filepath.Base(abs)
	if name == Reserved {
		return Profile{}, fmt.Errorf("profile %s: directory name %q is reserved for the clone's own templates", path, Reserved)
	}
	if strings.HasPrefix(name, "_") {
		return Profile{}, fmt.Errorf("profile %s: directory name %q starts with \"_\", which helm skips when rendering the profile's templates", path, name)
	}
	p, err := fromFS(os.DirFS(abs), name, false)
	if err != nil {
		return Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	p.Path, p.dir = path, abs
	return p, nil
}

func List(userDir string) ([]Profile, error) {
	names, err := availableNames(userDir)
	if err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(names))
	for _, name := range names {
		p, err := Load(userDir, name)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// availableNames returns the sorted, deduplicated set of profile names
// discoverable across the built-in and user directories, without parsing any
// profile.yaml. It is the single source of truth for "what profiles exist"
// used by both List and Load's unknown-name error, so neither has to load a
// profile just to find out its name is valid.
func availableNames(userDir string) ([]string, error) {
	byName := map[string]struct{}{}

	builtins, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("read embedded profiles: %w", err)
	}
	for _, e := range builtins {
		if e.IsDir() {
			byName[e.Name()] = struct{}{}
		}
	}

	entries, err := os.ReadDir(userDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", userDir, err)
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != Reserved {
			byName[e.Name()] = struct{}{}
		}
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
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
		seen := map[string]string{}
		for _, e := range valueFiles {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if !isYAML(e.Name()) {
				return Profile{}, fmt.Errorf("profile %q: values/%s is not a .yaml or .yml file; JSON content is valid YAML, so give it a .yaml name", name, e.Name())
			}
			chart := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".yaml"), ".yml")
			if prev, ok := seen[chart]; ok {
				return Profile{}, fmt.Errorf("profile %q: values/%s and values/%s both target %s", name, prev, e.Name(), chart)
			}
			seen[chart] = e.Name()
			raw, err := fs.ReadFile(fsys, path.Join("values", e.Name()))
			if err != nil {
				return Profile{}, fmt.Errorf("profile %q: read %s: %w", name, e.Name(), err)
			}
			var v map[string]any
			if err := yaml.Unmarshal(raw, &v); err != nil {
				return Profile{}, fmt.Errorf("profile %q: parse %s: %w", name, e.Name(), err)
			}
			p.Values[chart] = v
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
