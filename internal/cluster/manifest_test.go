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
