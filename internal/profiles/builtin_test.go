package profiles

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
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

// TestNFSProfileStorageClassMatchesDriverName pins that the nfs profile's
// StorageClass provisioner and its ceph-csi-drivers overlay's driver name
// agree, rather than hardcoding either literal: they registered a driver
// under one name while the StorageClass provisioner pointed at another,
// which pends every PVC forever.
func TestNFSProfileStorageClassMatchesDriverName(t *testing.T) {
	p, err := Load(t.TempDir(), "nfs")
	if err != nil {
		t.Fatal(err)
	}

	drivers, ok := p.Values["ceph-csi-drivers"]["drivers"].(map[string]any)
	if !ok {
		t.Fatalf("values = %#v", p.Values)
	}
	nfs, ok := drivers["nfs"].(map[string]any)
	if !ok {
		t.Fatalf("drivers.nfs = %#v", drivers["nfs"])
	}
	driverName, ok := nfs["name"].(string)
	if !ok || driverName == "" {
		t.Fatalf("drivers.nfs.name = %#v, want a non-empty string", nfs["name"])
	}

	var sc struct {
		Provisioner string `yaml:"provisioner"`
	}
	tmpl, ok := p.Templates["20-storageclass.yaml"]
	if !ok {
		t.Fatalf("no 20-storageclass.yaml template, have %v", p.Templates)
	}
	if err := yaml.Unmarshal(tmpl, &sc); err != nil {
		t.Fatalf("parse 20-storageclass.yaml: %v", err)
	}

	if sc.Provisioner != driverName {
		t.Errorf("StorageClass provisioner %q does not match ceph-csi-drivers nfs driver name %q", sc.Provisioner, driverName)
	}
}
