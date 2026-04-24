// Package unit_test -- T-07 unit tests for helm metadata fields on wrapper types.
//
// Tests cover:
//   - ClusterPackSpec, PackExecutionSpec, PackInstanceSpec all have ChartVersion,
//     ChartURL, ChartName, HelmVersion fields.
//   - Fields are optional/omitempty: absent when not set.
//   - JSON serialization round-trip preserves all four field values.
//   - Fields are absent in serialized output when zero-value (omitempty).
//
// T-07, Decision B, T-04 schema.
package unit_test

import (
	"encoding/json"
	"testing"

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
)

func TestClusterPackSpec_HelmMetadataFields_RoundTrip(t *testing.T) {
	spec := seamcorev1alpha1.InfrastructureClusterPackSpec{
		Version: "v1.0.0",
		RegistryRef: seamcorev1alpha1.InfrastructurePackRegistryRef{
			URL:    "registry.example.com/packs/cert-manager",
			Digest: "sha256:abc123",
		},
		Checksum:     "deadbeef",
		ChartVersion: "v1.13.3",
		ChartURL:     "https://charts.jetstack.io",
		ChartName:    "cert-manager",
		HelmVersion:  "v3.14.0",
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal ClusterPackSpec: %v", err)
	}

	var got seamcorev1alpha1.InfrastructureClusterPackSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal ClusterPackSpec: %v", err)
	}

	if got.ChartVersion != spec.ChartVersion {
		t.Errorf("ChartVersion: got %q, want %q", got.ChartVersion, spec.ChartVersion)
	}
	if got.ChartURL != spec.ChartURL {
		t.Errorf("ChartURL: got %q, want %q", got.ChartURL, spec.ChartURL)
	}
	if got.ChartName != spec.ChartName {
		t.Errorf("ChartName: got %q, want %q", got.ChartName, spec.ChartName)
	}
	if got.HelmVersion != spec.HelmVersion {
		t.Errorf("HelmVersion: got %q, want %q", got.HelmVersion, spec.HelmVersion)
	}
}

func TestClusterPackSpec_HelmMetadataFields_AbsentWhenZero(t *testing.T) {
	spec := seamcorev1alpha1.InfrastructureClusterPackSpec{
		Version: "v1.0.0",
		RegistryRef: seamcorev1alpha1.InfrastructurePackRegistryRef{
			URL: "registry.example.com/packs/raw-pack",
		},
		Checksum: "deadbeef",
		// No ChartVersion, ChartURL, ChartName, HelmVersion -- kustomize/raw pack.
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal ClusterPackSpec: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	for _, field := range []string{"chartVersion", "chartURL", "chartName", "helmVersion"} {
		if _, ok := raw[field]; ok {
			t.Errorf("field %q present in JSON but should be absent (omitempty)", field)
		}
	}
}

func TestPackExecutionSpec_HelmMetadataFields_RoundTrip(t *testing.T) {
	spec := seamcorev1alpha1.InfrastructurePackExecutionSpec{
		ClusterPackRef: seamcorev1alpha1.InfrastructureClusterPackRef{
			Name:    "cert-manager-v1.13.3-r1",
			Version: "v1.13.3",
		},
		TargetClusterRef:    "ccs-mgmt",
		AdmissionProfileRef: "cert-manager",
		ChartVersion:        "v1.13.3",
		ChartURL:            "https://charts.jetstack.io",
		ChartName:           "cert-manager",
		HelmVersion:         "v3.14.0",
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal PackExecutionSpec: %v", err)
	}

	var got seamcorev1alpha1.InfrastructurePackExecutionSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal PackExecutionSpec: %v", err)
	}

	if got.ChartVersion != spec.ChartVersion {
		t.Errorf("ChartVersion: got %q, want %q", got.ChartVersion, spec.ChartVersion)
	}
	if got.ChartURL != spec.ChartURL {
		t.Errorf("ChartURL: got %q, want %q", got.ChartURL, spec.ChartURL)
	}
	if got.ChartName != spec.ChartName {
		t.Errorf("ChartName: got %q, want %q", got.ChartName, spec.ChartName)
	}
	if got.HelmVersion != spec.HelmVersion {
		t.Errorf("HelmVersion: got %q, want %q", got.HelmVersion, spec.HelmVersion)
	}
}

func TestPackExecutionSpec_HelmMetadataFields_AbsentWhenZero(t *testing.T) {
	spec := seamcorev1alpha1.InfrastructurePackExecutionSpec{
		ClusterPackRef: seamcorev1alpha1.InfrastructureClusterPackRef{
			Name:    "raw-pack-v1.0.0-r1",
			Version: "v1.0.0",
		},
		TargetClusterRef:    "ccs-mgmt",
		AdmissionProfileRef: "raw-pack",
		// No helm metadata -- raw or kustomize pack.
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal PackExecutionSpec: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	for _, field := range []string{"chartVersion", "chartURL", "chartName", "helmVersion"} {
		if _, ok := raw[field]; ok {
			t.Errorf("field %q present in JSON but should be absent (omitempty)", field)
		}
	}
}

func TestPackInstanceSpec_HelmMetadataFields_RoundTrip(t *testing.T) {
	spec := seamcorev1alpha1.InfrastructurePackInstanceSpec{
		ClusterPackRef:   "cert-manager-v1.13.3-r1",
		TargetClusterRef: "ccs-mgmt",
		ChartVersion:     "v1.13.3",
		ChartURL:         "https://charts.jetstack.io",
		ChartName:        "cert-manager",
		HelmVersion:      "v3.14.0",
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal PackInstanceSpec: %v", err)
	}

	var got seamcorev1alpha1.InfrastructurePackInstanceSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal PackInstanceSpec: %v", err)
	}

	if got.ChartVersion != spec.ChartVersion {
		t.Errorf("ChartVersion: got %q, want %q", got.ChartVersion, spec.ChartVersion)
	}
	if got.ChartURL != spec.ChartURL {
		t.Errorf("ChartURL: got %q, want %q", got.ChartURL, spec.ChartURL)
	}
	if got.ChartName != spec.ChartName {
		t.Errorf("ChartName: got %q, want %q", got.ChartName, spec.ChartName)
	}
	if got.HelmVersion != spec.HelmVersion {
		t.Errorf("HelmVersion: got %q, want %q", got.HelmVersion, spec.HelmVersion)
	}
}

func TestPackInstanceSpec_HelmMetadataFields_AbsentWhenZero(t *testing.T) {
	spec := seamcorev1alpha1.InfrastructurePackInstanceSpec{
		ClusterPackRef:   "raw-pack-v1.0.0-r1",
		TargetClusterRef: "ccs-mgmt",
		// No helm metadata -- raw or kustomize pack.
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal PackInstanceSpec: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	for _, field := range []string{"chartVersion", "chartURL", "chartName", "helmVersion"} {
		if _, ok := raw[field]; ok {
			t.Errorf("field %q present in JSON but should be absent (omitempty)", field)
		}
	}
}
