package cluster

import (
	"strings"
	"testing"
)

func TestGenerateConfigMountsUdevWithDisks(t *testing.T) {
	cfg := Config{
		Name:    "rook",
		Workers: 2,
		WorkerDisks: map[int][]Disk{
			0: {{HostPath: "/dev/sdb", ContainerPath: "/dev/sdb"}},
		},
	}
	out, err := GenerateConfig(cfg)
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}
	got := string(out)

	if !strings.Contains(got, "hostPath: /run/udev") {
		t.Errorf("expected /run/udev mount for a worker with disks; got:\n%s", got)
	}
	if !strings.Contains(got, "hostPath: /dev/sdb") {
		t.Errorf("expected the disk mount to be preserved; got:\n%s", got)
	}
}

func TestGenerateConfigNoUdevWithoutDisks(t *testing.T) {
	out, err := GenerateConfig(Config{Name: "rook", Workers: 2})
	if err != nil {
		t.Fatalf("GenerateConfig: %v", err)
	}
	if strings.Contains(string(out), "/run/udev") {
		t.Errorf("did not expect /run/udev mount when no worker has disks; got:\n%s", out)
	}
}
