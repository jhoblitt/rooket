package cmd

import (
	"reflect"
	"sort"
	"testing"

	"github.com/jhoblitt/rooket/internal/engine"
)

func TestScopeTeardownSet(t *testing.T) {
	engs := []engine.Engine{engine.Podman}
	live := map[string][]engine.Engine{
		"has-state":    engs, // live + state dir      -> owned
		"has-registry": engs, // live, no state, owns  -> owned
		"foreign":      engs, // live, no state, !owns  -> unmanaged
	}
	stateNames := []string{"has-state", "orphan-state"} // orphan-state: state only
	owns := func(name string, _ []engine.Engine) bool { return name == "has-registry" }

	t.Run("default excludes foreign", func(t *testing.T) {
		set, unmanaged := scopeTeardownSet(live, stateNames, false, owns)
		gotSet := keys(set)
		wantSet := []string{"has-registry", "has-state", "orphan-state"}
		if !reflect.DeepEqual(gotSet, wantSet) {
			t.Errorf("set = %v, want %v", gotSet, wantSet)
		}
		if !reflect.DeepEqual(unmanaged, []string{"foreign"}) {
			t.Errorf("unmanaged = %v, want [foreign]", unmanaged)
		}
	})

	t.Run("include-unmanaged sweeps foreign", func(t *testing.T) {
		set, unmanaged := scopeTeardownSet(live, stateNames, true, owns)
		gotSet := keys(set)
		wantSet := []string{"foreign", "has-registry", "has-state", "orphan-state"}
		if !reflect.DeepEqual(gotSet, wantSet) {
			t.Errorf("set = %v, want %v", gotSet, wantSet)
		}
		if len(unmanaged) != 0 {
			t.Errorf("unmanaged = %v, want none", unmanaged)
		}
	})

	t.Run("owns not consulted when state present", func(t *testing.T) {
		calledFor := map[string]bool{}
		spy := func(name string, e []engine.Engine) bool { calledFor[name] = true; return false }
		scopeTeardownSet(map[string][]engine.Engine{"has-state": engs}, []string{"has-state"}, false, spy)
		if calledFor["has-state"] {
			t.Error("owns() consulted for a cluster that already has a state dir")
		}
	})
}

func keys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
