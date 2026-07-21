package profiles

import (
	"strings"
	"testing"
)

func TestBuiltInProfilesLoad(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"rbd", "rgw", "nfs"} {
		t.Run(name, func(t *testing.T) {
			p, err := Load(dir, name)
			if err != nil {
				t.Fatal(err)
			}
			if !p.BuiltIn {
				t.Error("want BuiltIn")
			}
			if p.Description == "" {
				t.Error("want a description")
			}
			if len(p.Templates) == 0 {
				t.Error("want at least one template")
			}
			for file, data := range p.Templates {
				if !strings.Contains(string(data), "kind:") {
					t.Errorf("%s has no kind: %q", file, data)
				}
			}
		})
	}
}

func TestRBDProfileHasNoValuesOverlay(t *testing.T) {
	p, err := Load(t.TempDir(), "rbd")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Values) != 0 {
		t.Errorf("rbd should rely on the chart's default block pool, got %#v", p.Values)
	}
}

func TestNFSProfileEnablesTheDriver(t *testing.T) {
	p, err := Load(t.TempDir(), "nfs")
	if err != nil {
		t.Fatal(err)
	}
	drivers, ok := p.Values["ceph-csi-drivers"]["drivers"].(map[string]any)
	if !ok {
		t.Fatalf("values = %#v", p.Values)
	}
	if drivers["nfs"].(map[string]any)["enabled"] != true {
		t.Errorf("nfs driver not enabled: %#v", drivers["nfs"])
	}
}
