package engine

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		want    Engine
		wantErr bool
	}{
		{"podman", Podman, false},
		{"docker", Docker, false},
		{"", "", true},
		{"containerd", "", true},
		{"Podman", "", true}, // case-sensitive: must match the binary name exactly
	}
	for _, tc := range cases {
		got, err := Parse(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStringIsBinaryName(t *testing.T) {
	if Podman.String() != "podman" || Docker.String() != "docker" {
		t.Errorf("String() must yield the binary name; got %q and %q", Podman, Docker)
	}
}

func TestPushArgs(t *testing.T) {
	// podman needs --tls-verify=false for the local HTTP registry; docker
	// rejects that flag and treats localhost registries as insecure already.
	if got := strings.Join(Podman.PushArgs("localhost:5001/x:y"), " "); got != "push --tls-verify=false localhost:5001/x:y" {
		t.Errorf("podman PushArgs = %q", got)
	}
	if got := strings.Join(Docker.PushArgs("localhost:5001/x:y"), " "); got != "push localhost:5001/x:y" {
		t.Errorf("docker PushArgs = %q", got)
	}
}
