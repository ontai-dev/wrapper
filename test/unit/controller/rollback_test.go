package controller_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seamv1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

// fakePOR builds a PackOperationResult representing a deploy at the given revision.
// If superseded is true the ontai.dev/superseded=true label is set (retained history).
func fakePOR(name, namespace, cpName string, revision int64, version, rbacDigest, workloadDigest string, superseded bool) *seamv1alpha1.PackOperationResult {
	labels := map[string]string{
		"ontai.dev/cluster-pack":   cpName,
		"ontai.dev/pack-execution": cpName + "-ccs-dev",
	}
	if superseded {
		labels["ontai.dev/superseded"] = "true"
	}
	return &seamv1alpha1.PackOperationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: seamv1alpha1.PackOperationResultSpec{
			Revision:           revision,
			ClusterPackRef:     cpName,
			ClusterPackVersion: version,
			RBACDigest:         rbacDigest,
			WorkloadDigest:     workloadDigest,
			Capability:         "pack-deploy",
			Status:             seamv1alpha1.PackResultSucceeded,
		},
	}
}

// buildRollbackCP creates a ClusterPack with RollbackToRevision set to targetRevision.
func buildRollbackCP(name, version, namespace string, targetRevision int64, rbacDigest, workloadDigest string) *seamv1alpha1.InfrastructureClusterPack {
	cp := newSignedCP(name, version, namespace)
	cp.Spec.RBACDigest = rbacDigest
	cp.Spec.WorkloadDigest = workloadDigest
	cp.Spec.TargetClusters = []string{"ccs-dev"}
	cp.Spec.RollbackToRevision = targetRevision
	cp.Annotations["infrastructure.ontai.dev/spec-checksum-snapshot"] = "stale-checksum"
	return cp
}

// TestClusterPackReconciler_Rollback_OneStep verifies that rolling back one step (N to N-1)
// patches the spec from the superseded POR at revision N-1. wrapper-schema.md §6.2.
func TestClusterPackReconciler_Rollback_OneStep(t *testing.T) {
	scheme := buildTestScheme(t)

	// Active POR at revision 2.
	activePOR := fakePOR(
		"pack-deploy-result-nginx-ccs-dev-ccs-dev-r2",
		"seam-tenant-ccs-dev", "nginx-ccs-dev",
		2, "v4.10.0-r1", "sha256:cccc", "sha256:dddd", false,
	)
	// Superseded POR at revision 1 (retained for rollback).
	supersededPOR := fakePOR(
		"pack-deploy-result-nginx-ccs-dev-ccs-dev-r1",
		"seam-tenant-ccs-dev", "nginx-ccs-dev",
		1, "v4.9.0-r1", "sha256:aaaa", "sha256:bbbb", true,
	)

	// ClusterPack at current version v4.10.0-r1, requesting rollback to revision 1.
	cp := buildRollbackCP("nginx-ccs-dev", "v4.10.0-r1", "seam-tenant-ccs-dev", 1, "sha256:cccc", "sha256:dddd")
	cp.UID = types.UID("uid-cp-nginx")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cp, activePOR, supersededPOR).
		WithStatusSubresource(cp).
		Build()

	reconciler := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nginx-ccs-dev", Namespace: "seam-tenant-ccs-dev"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &seamv1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(),
		client.ObjectKey{Name: "nginx-ccs-dev", Namespace: "seam-tenant-ccs-dev"},
		updated); err != nil {
		t.Fatalf("Get ClusterPack after rollback: %v", err)
	}

	if updated.Spec.Version != "v4.9.0-r1" {
		t.Errorf("spec.version=%q, want v4.9.0-r1", updated.Spec.Version)
	}
	if updated.Spec.RBACDigest != "sha256:aaaa" {
		t.Errorf("spec.rbacDigest=%q, want sha256:aaaa", updated.Spec.RBACDigest)
	}
	if updated.Spec.WorkloadDigest != "sha256:bbbb" {
		t.Errorf("spec.workloadDigest=%q, want sha256:bbbb", updated.Spec.WorkloadDigest)
	}
	if updated.Spec.RollbackToRevision != 0 {
		t.Errorf("spec.rollbackToRevision=%d, want 0 (cleared)", updated.Spec.RollbackToRevision)
	}
	if _, ok := updated.Annotations["infrastructure.ontai.dev/spec-checksum-snapshot"]; ok {
		t.Error("spec-checksum-snapshot annotation should be removed after rollback")
	}
}

// TestClusterPackReconciler_Rollback_NStep verifies that N-step rollback works --
// rolling back from revision 3 directly to revision 1 reads the correct artifact
// state from the retained superseded POR. wrapper-schema.md §6.2.
func TestClusterPackReconciler_Rollback_NStep(t *testing.T) {
	scheme := buildTestScheme(t)

	// Active POR at revision 3.
	activePOR := fakePOR(
		"pack-deploy-result-nginx-ccs-dev-ccs-dev-r3",
		"seam-tenant-ccs-dev", "nginx-ccs-dev",
		3, "v4.11.0-r1", "sha256:eeee", "sha256:ffff", false,
	)
	// Superseded POR at revision 2.
	supersededR2 := fakePOR(
		"pack-deploy-result-nginx-ccs-dev-ccs-dev-r2",
		"seam-tenant-ccs-dev", "nginx-ccs-dev",
		2, "v4.10.0-r1", "sha256:cccc", "sha256:dddd", true,
	)
	// Superseded POR at revision 1.
	supersededR1 := fakePOR(
		"pack-deploy-result-nginx-ccs-dev-ccs-dev-r1",
		"seam-tenant-ccs-dev", "nginx-ccs-dev",
		1, "v4.9.0-r1", "sha256:aaaa", "sha256:bbbb", true,
	)

	// ClusterPack at v4.11.0-r1, requesting direct rollback to revision 1 (two steps).
	cp := buildRollbackCP("nginx-ccs-dev", "v4.11.0-r1", "seam-tenant-ccs-dev", 1, "sha256:eeee", "sha256:ffff")
	cp.UID = types.UID("uid-cp-nginx")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cp, activePOR, supersededR2, supersededR1).
		WithStatusSubresource(cp).
		Build()

	reconciler := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nginx-ccs-dev", Namespace: "seam-tenant-ccs-dev"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &seamv1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(),
		client.ObjectKey{Name: "nginx-ccs-dev", Namespace: "seam-tenant-ccs-dev"},
		updated); err != nil {
		t.Fatalf("Get ClusterPack after N-step rollback: %v", err)
	}

	if updated.Spec.Version != "v4.9.0-r1" {
		t.Errorf("spec.version=%q, want v4.9.0-r1", updated.Spec.Version)
	}
	if updated.Spec.RBACDigest != "sha256:aaaa" {
		t.Errorf("spec.rbacDigest=%q, want sha256:aaaa", updated.Spec.RBACDigest)
	}
	if updated.Spec.WorkloadDigest != "sha256:bbbb" {
		t.Errorf("spec.workloadDigest=%q, want sha256:bbbb", updated.Spec.WorkloadDigest)
	}
	if updated.Spec.RollbackToRevision != 0 {
		t.Errorf("spec.rollbackToRevision=%d, want 0 (cleared)", updated.Spec.RollbackToRevision)
	}
}

// TestClusterPackReconciler_Rollback_NoPOR_ClearsField verifies that when no POR
// exists for the ClusterPack, the reconciler clears rollbackToRevision without error.
func TestClusterPackReconciler_Rollback_NoPOR_ClearsField(t *testing.T) {
	scheme := buildTestScheme(t)

	cp := buildRollbackCP("nginx-ccs-dev", "v4.10.0-r1", "seam-tenant-ccs-dev", 1, "sha256:cccc", "sha256:dddd")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cp).
		WithStatusSubresource(cp).
		Build()

	reconciler := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nginx-ccs-dev", Namespace: "seam-tenant-ccs-dev"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &seamv1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(),
		client.ObjectKey{Name: "nginx-ccs-dev", Namespace: "seam-tenant-ccs-dev"},
		updated); err != nil {
		t.Fatalf("Get ClusterPack: %v", err)
	}
	if updated.Spec.RollbackToRevision != 0 {
		t.Errorf("rollbackToRevision=%d, want 0", updated.Spec.RollbackToRevision)
	}
}

// TestClusterPackReconciler_Rollback_RevisionNotFound_ClearsField verifies that when
// rollbackToRevision references a revision not present in the retained POR history
// (e.g., already pruned or never written), the field is cleared without patching spec.
func TestClusterPackReconciler_Rollback_RevisionNotFound_ClearsField(t *testing.T) {
	scheme := buildTestScheme(t)

	// Only revision 3 exists (active). Revisions 1 and 2 are not retained.
	activePOR := fakePOR(
		"pack-deploy-result-nginx-ccs-dev-ccs-dev-r3",
		"seam-tenant-ccs-dev", "nginx-ccs-dev",
		3, "v4.11.0-r1", "sha256:eeee", "sha256:ffff", false,
	)
	// Request rollback to revision 1 which does not exist in history.
	cp := buildRollbackCP("nginx-ccs-dev", "v4.11.0-r1", "seam-tenant-ccs-dev", 1, "sha256:eeee", "sha256:ffff")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cp, activePOR).
		WithStatusSubresource(cp).
		Build()

	reconciler := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nginx-ccs-dev", Namespace: "seam-tenant-ccs-dev"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &seamv1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(),
		client.ObjectKey{Name: "nginx-ccs-dev", Namespace: "seam-tenant-ccs-dev"},
		updated); err != nil {
		t.Fatalf("Get ClusterPack: %v", err)
	}
	if updated.Spec.RollbackToRevision != 0 {
		t.Errorf("rollbackToRevision=%d, want 0", updated.Spec.RollbackToRevision)
	}
	// Version must be unchanged (no spec patch should have happened).
	if updated.Spec.Version != "v4.11.0-r1" {
		t.Errorf("spec.version=%q, want v4.11.0-r1 (unchanged)", updated.Spec.Version)
	}
}
