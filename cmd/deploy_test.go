package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeChartYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Chart.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCephCsiOperatorDep(t *testing.T) {
	t.Run("v1.20+/master style: installCsiOperator condition", func(t *testing.T) {
		p := writeChartYAML(t, `apiVersion: v2
name: rook-ceph
version: 0.0.1
dependencies:
  - name: library
    version: "0.0.1"
    repository: "file://../library"
  - name: ceph-csi-operator
    version: 1.0.1
    repository: https://ceph.github.io/ceph-csi-operator
    alias: ceph-csi-operator
    condition: csi.installCsiOperator
`)
		v, c, err := cephCsiOperatorDep(p)
		if err != nil || v != "1.0.1" || c != "csi.installCsiOperator" {
			t.Fatalf("got (%q, %q, %v), want (1.0.1, csi.installCsiOperator, nil)", v, c, err)
		}
	})

	t.Run("v1.18/v1.19 style: rookUseCsiOperator condition", func(t *testing.T) {
		p := writeChartYAML(t, `dependencies:
  - name: ceph-csi-operator
    version: "0.6.0"
    repository: https://ceph.github.io/ceph-csi-operator
    alias: ceph-csi-operator
    condition: csi.rookUseCsiOperator
`)
		v, c, err := cephCsiOperatorDep(p)
		if err != nil || v != "0.6.0" || c != "csi.rookUseCsiOperator" {
			t.Fatalf("got (%q, %q, %v), want (0.6.0, csi.rookUseCsiOperator, nil)", v, c, err)
		}
	})

	t.Run("no ceph-csi-operator dependency", func(t *testing.T) {
		p := writeChartYAML(t, `dependencies:
  - name: library
    version: "0.0.1"
    repository: "file://../library"
`)
		v, c, err := cephCsiOperatorDep(p)
		if err != nil || v != "" || c != "" {
			t.Fatalf("got (%q, %q, %v), want empty fields and nil error", v, c, err)
		}
	})

	t.Run("other dependency's fields not picked up", func(t *testing.T) {
		p := writeChartYAML(t, `dependencies:
  - name: ceph-csi-operator
    version: 1.2.3
    condition: csi.installCsiOperator
  - name: something-else
    version: "9.9.9"
    condition: other.flag
`)
		v, c, err := cephCsiOperatorDep(p)
		if err != nil || v != "1.2.3" || c != "csi.installCsiOperator" {
			t.Fatalf("got (%q, %q, %v), want (1.2.3, csi.installCsiOperator, nil)", v, c, err)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		if _, _, err := cephCsiOperatorDep(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
			t.Fatal("got nil error for missing file")
		}
	})
}

func TestClusterStorageNodesFromDisks(t *testing.T) {
	got, err := clusterStorageNodes("c", 2, 1, func(iqn string) (string, error) {
		return "/dev/disk/by-path/" + iqn, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Name != "c-worker" || got[1].Name != "c-worker2" {
		t.Errorf("node names = %q, %q", got[0].Name, got[1].Name)
	}
	if len(got[0].Devices) != 1 {
		t.Errorf("devices = %#v", got[0].Devices)
	}
}

func TestClusterStorageNodesUnresolvedDeviceErrors(t *testing.T) {
	resolveErr := errors.New("boom")
	got, err := clusterStorageNodes("c", 2, 1, func(iqn string) (string, error) {
		return "", resolveErr
	})
	if err == nil {
		t.Fatal("got nil error, want an error from the unresolved device")
	}
	if !errors.Is(err, resolveErr) {
		t.Errorf("error %v does not wrap %v", err, resolveErr)
	}
	if got != nil {
		t.Errorf("got %#v nodes, want nil", got)
	}
}

func TestHelmValueArgs(t *testing.T) {
	t.Run("no sets", func(t *testing.T) {
		got := helmValueArgs("/values/operator.yaml", nil)
		want := []string{"-f", "/values/operator.yaml"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})

	t.Run("sets follow -f as separate pairs", func(t *testing.T) {
		got := helmValueArgs("/values/operator.yaml", []string{"a.b=1", "c=2"})
		want := []string{"-f", "/values/operator.yaml", "--set", "a.b=1", "--set", "c=2"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})
}
