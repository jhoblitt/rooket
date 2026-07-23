package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jhoblitt/rooket/internal/clone"
	"github.com/jhoblitt/rooket/internal/profiles"
)

func TestProfileSourcesOrdersCloneFirst(t *testing.T) {
	root := t.TempDir()
	d := clone.Open(root)
	if err := d.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".rooket", "templates", "scratch.yaml"),
		[]byte("kind: PersistentVolumeClaim\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := profileSources(d, []profiles.Profile{
		{Name: "rgw", Templates: map[string][]byte{"20-obc.yaml": []byte("kind: ObjectBucketClaim\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Prefix != "local" {
		t.Errorf("first prefix = %q, want local", got[0].Prefix)
	}
	if got[1].Prefix != "rgw" {
		t.Errorf("second prefix = %q, want rgw", got[1].Prefix)
	}
}

func TestProfileSourcesSkipsEmptyClone(t *testing.T) {
	got, err := profileSources(clone.Open(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want none", got)
	}
}

func TestProfileSourcesSkipsEmptyProfileTemplates(t *testing.T) {
	got, err := profileSources(clone.Open(t.TempDir()), []profiles.Profile{
		{Name: "rgw", Templates: map[string][]byte{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want none", got)
	}
}

func TestProfileSourcesOrdersMultipleProfiles(t *testing.T) {
	got, err := profileSources(clone.Open(t.TempDir()), []profiles.Profile{
		{Name: "rgw", Templates: map[string][]byte{"20-obc.yaml": []byte("kind: ObjectBucketClaim\n")}},
		{Name: "cephfs", Templates: map[string][]byte{"10-pvc.yaml": []byte("kind: PersistentVolumeClaim\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %#v", got)
	}
	if got[0].Prefix != "rgw" {
		t.Errorf("first prefix = %q, want rgw", got[0].Prefix)
	}
	if got[1].Prefix != "cephfs" {
		t.Errorf("second prefix = %q, want cephfs", got[1].Prefix)
	}
}

func TestProfilesReleaseArgs(t *testing.T) {
	const kubeContext = "test-ctx"
	const chartDir = "/state/profiles-chart"

	cases := []struct {
		name string
		any  bool
		want []string
	}{
		{
			name: "uninstall when nothing contributed",
			any:  false,
			want: []string{
				"--kube-context", kubeContext, "-n", "rook-ceph",
				"uninstall", profilesRelease, "--ignore-not-found",
			},
		},
		{
			name: "upgrade install when something contributed",
			any:  true,
			want: []string{
				"--kube-context", kubeContext, "-n", "rook-ceph",
				"upgrade", "--install", profilesRelease, chartDir,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := profilesReleaseArgs(tc.any, kubeContext, chartDir)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("profilesReleaseArgs(%v, ...) = %#v, want %#v", tc.any, got, tc.want)
			}
		})
	}
}
