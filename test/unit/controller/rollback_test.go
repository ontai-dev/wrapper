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

// fakePOR builds a PackOperationResult with rollback anchor fields pre-populated.
func fakePOR(name, namespace, cpName, version, rbacDigest, workloadDigest string, revision int64, prevVersion, prevRBAC, prevWorkload string) *seamv1alpha1.PackOperationResult {
	return &seamv1alpha1.PackOperationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"ontai.dev/cluster-pack":   cpName,
				"ontai.dev/pack-execution": cpName + "-ccs-dev",
			},
		},
		Spec: seamv1alpha1.PackOperationResultSpec{
			Revision:                   revision,
			ClusterPackRef:             cpName,
			ClusterPackVersion:         version,
			RBACDigest:                 rbacDigest,
			WorkloadDigest:             workloadDigest,
			PreviousClusterPackVersion: prevVersion,
			PreviousRBACDigest:         prevRBAC,
			PreviousWorkloadDigest:     prevWorkload,
			Capability:                 "pack-deploy",
			Status:                     seamv1alpha1.PackResultSucceeded,
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

// TestClusterPackReconciler_Rollback_PatchesSpecAndClearsField verifies that when
// spec.rollbackToRevision == currentPOR.revision - 1, the reconciler patches the
// ClusterPack spec back to the previous version/digests, clears rollbackToRevision,
// and removes the spec-snapshot annotation. wrapper-schema.md §6.2.
func TestClusterPackReconciler_Rollback_PatchesSpecAndClearsField(t *testing.T) {
	scheme := buildTestScheme(t)

	// Current POR at revision 2: current=v4.10.0-r1, previous=v4.9.0-r1.
	por := fakePOR(
		"pack-deploy-result-nginx-ccs-dev-ccs-dev-r2",
		"seam-tenant-ccs-dev",
		"nginx-ccs-dev",
		"v4.10.0-r1", "sha256:cccc", "sha256:dddd",
		2,
		"v4.9.0-r1", "sha256:aaaa", "sha256:bbbb",
	)

	// ClusterPack at current version v4.10.0-r1, requesting rollback to revision 1.
	cp := buildRollbackCP("nginx-ccs-dev", "v4.10.0-r1", "seam-tenant-ccs-dev", 1, "sha256:cccc", "sha256:dddd")
	cp.UID = types.UID("uid-cp-nginx")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cp, por).
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

	// Read the patched ClusterPack.
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

// TestClusterPackReconciler_Rollback_WrongRevision_ClearsField verifies that when
// rollbackToRevision != currentRevision-1 (non-one-step), the field is cleared without patching spec.
func TestClusterPackReconciler_Rollback_WrongRevision_ClearsField(t *testing.T) {
	scheme := buildTestScheme(t)

	// POR is at revision 3; request is to rollback to revision 1 (skips revision 2 -- not allowed).
	por := fakePOR(
		"pack-deploy-result-nginx-ccs-dev-ccs-dev-r3",
		"seam-tenant-ccs-dev",
		"nginx-ccs-dev",
		"v4.11.0-r1", "sha256:eeee", "sha256:ffff",
		3,
		"v4.10.0-r1", "sha256:cccc", "sha256:dddd",
	)
	cp := buildRollbackCP("nginx-ccs-dev", "v4.11.0-r1", "seam-tenant-ccs-dev", 1, "sha256:eeee", "sha256:ffff")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cp, por).
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
