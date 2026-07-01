package engine

import (
	"errors"
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

// fakeProber simulates the host's engines. podmanErr/dockerErr stand in for an
// engine that is not installed or whose backend is unreachable; rootless is the
// value podman's Rootless info field reports when podman is usable.
func fakeProber(podmanErr error, rootless bool, dockerErr error) Prober {
	return func(name string, args ...string) (string, error) {
		switch Engine(name) {
		case Podman:
			if podmanErr != nil {
				return "", podmanErr
			}
			for _, a := range args {
				if strings.Contains(a, "Rootless") {
					if rootless {
						return "true", nil
					}
					return "false", nil
				}
			}
			return "ok", nil
		case Docker:
			if dockerErr != nil {
				return "", dockerErr
			}
			return "ok", nil
		}
		return "", errors.New("unexpected engine probe: " + name)
	}
}

func TestResolve(t *testing.T) {
	missing := errors.New("not found")

	cases := []struct {
		name      string
		requested Engine
		explicit  bool
		probe     Prober
		want      Engine
		wantErr   bool
		wantWarn  bool
	}{
		{"rootful podman is used as-is", Podman, false, fakeProber(nil, false, nil), Podman, false, false},
		{"rootless podman falls back to docker", Podman, false, fakeProber(nil, true, nil), Docker, false, true},
		{"rootless podman with no docker errors", Podman, false, fakeProber(nil, true, missing), "", true, true},
		{"missing podman falls back to docker", Podman, false, fakeProber(missing, false, nil), Docker, false, true},
		{"no usable engine errors", Podman, false, fakeProber(missing, false, missing), "", true, true},
		{"docker requested and usable", Docker, true, fakeProber(missing, false, nil), Docker, false, false},
		{"docker requested but unusable errors", Docker, true, fakeProber(missing, false, missing), "", true, false},
		{"explicit rootful podman is used as-is", Podman, true, fakeProber(nil, false, nil), Podman, false, false},
		{"explicit rootless podman errors without fallback", Podman, true, fakeProber(nil, true, nil), "", true, false},
		{"explicit missing podman errors without fallback", Podman, true, fakeProber(missing, false, nil), "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warned bool
			got, err := Resolve(tc.requested, tc.explicit, tc.probe, func(string) { warned = true })
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got engine %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("engine = %q, want %q", got, tc.want)
			}
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v", warned, tc.wantWarn)
			}
		})
	}
}
