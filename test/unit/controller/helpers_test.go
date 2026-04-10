// Package controller_test provides shared test helpers for wrapper controller unit tests.
//
// Workstream 1 (PackInstance lifecycle) and Workstream 2 (Kueue Job scheduling)
// share helpers defined here. All tests use the controller-runtime fake client.
// No live cluster or Kueue admission is required.
package controller_test

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
)

// buildTestScheme returns a scheme with clientgoscheme (includes batch/v1, core/v1)
// and infrav1alpha1 registered.
func buildTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme clientgo: %v", err)
	}
	if err := infrav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme infrav1alpha1: %v", err)
	}
	return s
}

func boolPtr(b bool) *bool { return &b }

// newSignedCP returns a signed ClusterPack in the given namespace with a stable UID.
// Status.Signed=true and PackSignature are pre-set so the PackExecutionReconciler
// signature gate passes without running the ClusterPackReconciler.
func newSignedCP(name, version, namespace string) *infrav1alpha1.ClusterPack {
	return &infrav1alpha1.ClusterPack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("uid-cp-" + name),
			Annotations: map[string]string{
				"ontai.dev/pack-signature": "valid-sig==",
			},
		},
		Spec: infrav1alpha1.ClusterPackSpec{
			Version: version,
			RegistryRef: infrav1alpha1.PackRegistryRef{
				URL:    "registry.ontai.dev/packs/" + name,
				Digest: "sha256:abc123",
			},
			Checksum: "sha256:def456",
		},
		Status: infrav1alpha1.ClusterPackStatus{
			Signed:        true,
			PackSignature: "valid-sig==",
		},
	}
}

// newPE returns a PackExecution in the given namespace with an ownerReference
// pointing to the ClusterPack identified by cpName/cpUID.
func newPE(name, cpName, cpVersion string, cpUID types.UID, clusterRef, profileRef, namespace string) *infrav1alpha1.PackExecution {
	return &infrav1alpha1.PackExecution{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("uid-pe-" + name),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         infrav1alpha1.GroupVersion.String(),
				Kind:               "ClusterPack",
				Name:               cpName,
				UID:                cpUID,
				Controller:         boolPtr(true),
				BlockOwnerDeletion: boolPtr(true),
			}},
		},
		Spec: infrav1alpha1.PackExecutionSpec{
			ClusterPackRef: infrav1alpha1.ClusterPackRef{
				Name:    cpName,
				Version: cpVersion,
			},
			TargetClusterRef:    clusterRef,
			AdmissionProfileRef: profileRef,
		},
	}
}

// newRunnerConfig returns an unstructured RunnerConfig in ont-system with
// capCount capability entries pre-populated in status.capabilities.
// capCount=0 produces a RunnerConfig with an empty (absent) capabilities list,
// which gate 0 treats as "conductor not yet ready". conductor-schema.md §5.
func newRunnerConfig(clusterRef string, capCount int) *unstructured.Unstructured {
	rc := &unstructured.Unstructured{}
	rc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "runner.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RunnerConfig",
	})
	rc.SetName(clusterRef)
	rc.SetNamespace("ont-system")
	if capCount > 0 {
		caps := make([]interface{}, capCount)
		for i := 0; i < capCount; i++ {
			caps[i] = map[string]interface{}{
				"name":    "pack-deploy",
				"version": "v1.0.0",
			}
		}
		_ = unstructured.SetNestedSlice(rc.Object, caps, "status", "capabilities")
	}
	return rc
}

// newTalosCluster returns an unstructured TalosCluster in seam-tenant-{clusterRef}
// with ConductorReady condition set per the conductorReady argument.
// Reads via unstructured to avoid importing platform types (same pattern as reconciler).
func newTalosCluster(clusterRef string, conductorReady bool) *unstructured.Unstructured {
	tc := &unstructured.Unstructured{}
	tc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "platform.ontai.dev",
		Version: "v1alpha1",
		Kind:    "TalosCluster",
	})
	tc.SetName(clusterRef)
	tc.SetNamespace("seam-tenant-" + clusterRef)
	status := "False"
	if conductorReady {
		status = "True"
	}
	_ = unstructured.SetNestedSlice(tc.Object, []interface{}{
		map[string]interface{}{
			"type":   "ConductorReady",
			"status": status,
			"reason": "ConductorDeploymentAvailable",
		},
	}, "status", "conditions")
	return tc
}

// newPermissionSnapshot returns an unstructured PermissionSnapshot.
// name is the TargetClusterRef (reconciler looks up PS by clusterRef name).
func newPermissionSnapshot(name, namespace string, current bool) *unstructured.Unstructured {
	ps := &unstructured.Unstructured{}
	ps.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "PermissionSnapshot",
	})
	ps.SetName(name)
	ps.SetNamespace(namespace)
	status := "False"
	if current {
		status = "True"
	}
	_ = unstructured.SetNestedSlice(ps.Object, []interface{}{
		map[string]interface{}{
			"type":   "Current",
			"status": status,
			"reason": "SnapshotCurrent",
		},
	}, "status", "conditions")
	return ps
}

// newRBACProfile returns an unstructured RBACProfile with provisioned field set.
// name is the AdmissionProfileRef (reconciler looks up by that name).
func newRBACProfile(name, namespace string, provisioned bool) *unstructured.Unstructured {
	rp := &unstructured.Unstructured{}
	rp.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RBACProfile",
	})
	rp.SetName(name)
	rp.SetNamespace(namespace)
	_ = unstructured.SetNestedField(rp.Object, provisioned, "status", "provisioned")
	return rp
}

// newJob returns a batchv1.Job with the given completion counters.
func newJob(name, namespace string, succeeded, failed int32) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: batchv1.JobStatus{
			Succeeded: succeeded,
			Failed:    failed,
		},
	}
}

// newOperationResultCM returns the OperationResult ConfigMap written by the
// conductor executor after pack-deploy Job success. Name follows the convention
// "pack-deploy-result-{peName}".
func newOperationResultCM(peName, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pack-deploy-result-" + peName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"result": "success",
		},
	}
}

// packDeployJobName returns the deterministic Job name for a PackExecution.
// Mirrors the internal packDeployJobName function in the reconciler.
func packDeployJobName(peName string) string {
	return "pack-deploy-" + peName
}
