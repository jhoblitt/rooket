package cluster

import (
	"reflect"
	"testing"
)

func TestManifestCRDNames(t *testing.T) {
	t.Run("canonical and quoted CRD docs among other kinds", func(t *testing.T) {
		manifest := `---
# Source: prometheus-operator-crds/charts/crds/templates/crd-alertmanagers.yaml
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  annotations:
    controller-gen.kubebuilder.io/version: v0.18.0
  name: alertmanagers.monitoring.coreos.com
spec:
  group: monitoring.coreos.com
  names:
    kind: Alertmanager
    plural: alertmanagers
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: not-a-crd
data:
  k: v
---
apiVersion: apiextensions.k8s.io/v1
kind: "CustomResourceDefinition"
metadata: {name: servicemonitors.monitoring.coreos.com}
spec:
  names:
    kind: ServiceMonitor
`
		want := []string{
			"alertmanagers.monitoring.coreos.com",
			"servicemonitors.monitoring.coreos.com",
		}
		got, err := manifestCRDNames(manifest)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got (%v, %v), want (%v, nil)", got, err, want)
		}
	})

	t.Run("empty manifest", func(t *testing.T) {
		got, err := manifestCRDNames("")
		if err != nil || len(got) != 0 {
			t.Fatalf("got (%v, %v), want none and nil error", got, err)
		}
	})

	t.Run("CRD without a name errors", func(t *testing.T) {
		if _, err := manifestCRDNames("kind: CustomResourceDefinition\nmetadata: {}\n"); err == nil {
			t.Fatal("got nil error for CRD without metadata.name")
		}
	})

	t.Run("undecodable document errors", func(t *testing.T) {
		if _, err := manifestCRDNames("kind: [broken\n"); err == nil {
			t.Fatal("got nil error for undecodable manifest")
		}
	})
}

// TestParseDeployedChart pins the release lookup that decides whether a cluster
// predates the release rename — and so whether its CRDs are about to be
// installed under a name their helm ownership annotation does not match.
func TestParseDeployedChart(t *testing.T) {
	const list = `[
  {"name":"my-prometheus-operator-crds","namespace":"rook-ceph","revision":"1","status":"deployed","chart":"prometheus-operator-crds-29.0.0","app_version":"v0.91.0"}
]`
	if got := parseDeployedChart(list, "my-prometheus-operator-crds"); got != "prometheus-operator-crds-29.0.0" {
		t.Errorf("chart = %q, want prometheus-operator-crds-29.0.0", got)
	}
	// helm's --filter is a regex it applies loosely, so a name that merely
	// resembles the one asked for must not answer for it.
	if got := parseDeployedChart(list, "prometheus-operator-crds"); got != "" {
		t.Errorf("a different release answered the lookup: %q", got)
	}
	// Only a deployed release counts: a failed or pending one owns nothing and
	// must not suppress the install.
	const failed = `[{"name":"prometheus-operator-crds","status":"failed","chart":"prometheus-operator-crds-29.0.0"}]`
	if got := parseDeployedChart(failed, "prometheus-operator-crds"); got != "" {
		t.Errorf("a %s release must not count as deployed: %q", "failed", got)
	}
	for _, empty := range []string{"[]", "", "not json"} {
		if got := parseDeployedChart(empty, "prometheus-operator-crds"); got != "" {
			t.Errorf("parseDeployedChart(%q) = %q, want empty", empty, got)
		}
	}
}
