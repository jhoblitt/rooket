package cmd

import "testing"

func TestUpForwardsValueFlags(t *testing.T) {
	t.Cleanup(func() {
		upWith, upWithOnly, upValueFiles, upSets = nil, nil, nil, nil
		deployWith, deployWithOnly, deployValueFiles, deploySets = nil, nil, nil, nil
		deployWithOnlySet = false
	})

	upWith = []string{"rgw"}
	upWithOnly = []string{"rbd"}
	upValueFiles = []string{"/tmp/x.yaml"}
	upSets = []string{"a=b"}

	applyUpValueFlags(true)

	if len(deployWith) != 1 || deployWith[0] != "rgw" {
		t.Errorf("deployWith = %#v", deployWith)
	}
	if len(deployWithOnly) != 1 || deployWithOnly[0] != "rbd" {
		t.Errorf("deployWithOnly = %#v", deployWithOnly)
	}
	if !deployWithOnlySet {
		t.Error("deployWithOnlySet not propagated")
	}
	if len(deployValueFiles) != 1 {
		t.Errorf("deployValueFiles = %#v", deployValueFiles)
	}
	if len(deploySets) != 1 || deploySets[0] != "a=b" {
		t.Errorf("deploySets = %#v", deploySets)
	}
}
