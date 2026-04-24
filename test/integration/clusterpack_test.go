package integration_test

// Scenario 1 — ClusterPack accepted by real API server; reconciler sets
//              SignaturePending=True condition.
// Scenario 3 — PackInstance with ownerRef to PackExecution; delete PE;
//              confirm GC cascade removes PI.

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/seam-core/pkg/conditions"
)

// newClusterPack constructs a minimal valid ClusterPack for testing.
func newClusterPack(name, namespace string) *seamcorev1alpha1.InfrastructureClusterPack {
	return &seamcorev1alpha1.InfrastructureClusterPack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: seamcorev1alpha1.InfrastructureClusterPackSpec{
			Version: "v1.0.0",
			RegistryRef: seamcorev1alpha1.InfrastructurePackRegistryRef{
				URL:    "registry.example.com/packs/" + name,
				Digest: "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abc123",
			},
			Checksum: "sha256:deadbeef",
		},
	}
}

// TestClusterPack_AcceptedByAPIServer_SetsSignaturePending verifies that a
// structurally valid ClusterPack CR is:
//  1. Accepted by the real API server (persists in etcd).
//  2. Reconciled by ClusterPackReconciler which sets SignaturePending=True
//     because no pack signature annotation is present on creation.
//
// This validates the "unsigned pack enters pending state" lifecycle path
// against a real API server, which fake clients cannot validate (they don't
// enforce CRD schema or trigger controller reconciliation through a real
// informer watch).
//
// Scenario 1 — Test Session F.
func TestClusterPack_AcceptedByAPIServer_SetsSignaturePending(t *testing.T) {
	ctx := context.Background()
	ns := "seam-system"

	cp := newClusterPack("integration-test-pack", ns)
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("Create ClusterPack: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cp) })

	// Verify the object persists in etcd.
	nn := types.NamespacedName{Name: cp.Name, Namespace: ns}
	got := &seamcorev1alpha1.InfrastructureClusterPack{}
	if err := k8sClient.Get(ctx, nn, got); err != nil {
		t.Fatalf("ClusterPack not found in etcd after Create: %v", err)
	}

	// Wait for the reconciler to set SignaturePending=True.
	// The reconciler sets this condition when no pack signature annotation is
	// present (the pack has not yet been signed by the management cluster conductor).
	ok := poll(t, 10*time.Second, func() bool {
		got := &seamcorev1alpha1.InfrastructureClusterPack{}
		if err := k8sClient.Get(ctx, nn, got); err != nil {
			return false
		}
		c := conditions.FindCondition(got.Status.Conditions, conditions.ConditionTypeClusterPackSignaturePending)
		return c != nil && c.Status == metav1.ConditionTrue
	})
	if !ok {
		got := &seamcorev1alpha1.InfrastructureClusterPack{}
		_ = k8sClient.Get(ctx, nn, got)
		t.Errorf("timed out waiting for SignaturePending=True; conditions: %v", got.Status.Conditions)
	}
}

// TestClusterPack_ObservedGenerationAdvances verifies that after reconciliation
// the ClusterPack status.observedGeneration matches metadata.generation in the
// real API server. This cannot be verified with fake clients because fake clients
// do not auto-advance generation on mutation.
func TestClusterPack_ObservedGenerationAdvances(t *testing.T) {
	ctx := context.Background()
	ns := "seam-system"

	cp := newClusterPack("integration-observedgen-pack", ns)
	if err := k8sClient.Create(ctx, cp); err != nil {
		t.Fatalf("Create ClusterPack: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, cp) })

	nn := types.NamespacedName{Name: cp.Name, Namespace: ns}
	ok := poll(t, 10*time.Second, func() bool {
		got := &seamcorev1alpha1.InfrastructureClusterPack{}
		if err := k8sClient.Get(ctx, nn, got); err != nil {
			return false
		}
		return got.Status.ObservedGeneration == got.Generation && got.Generation > 0
	})
	if !ok {
		got := &seamcorev1alpha1.InfrastructureClusterPack{}
		_ = k8sClient.Get(ctx, nn, got)
		t.Errorf("timed out: ObservedGeneration=%d Generation=%d",
			got.Status.ObservedGeneration, got.Generation)
	}
}

// TestPackInstance_OwnerRefCascade_DeletedWhenPackExecutionDeleted verifies the
// Kubernetes garbage collection cascade: a PackInstance with BlockOwnerDeletion=true
// ownerReference pointing to a PackExecution is deleted automatically when the
// PackExecution is deleted.
//
// This is the critical finalizer ordering test that cannot be validated with fake
// clients — the fake client does not implement ownerReference garbage collection.
// The real API server's GC controller removes the owned object after the owner
// is gone.
//
// Scenario 2 — Test Session F. wrapper-schema.md §3.
func TestPackInstance_OwnerRefCascade_DeletedWhenPackExecutionDeleted(t *testing.T) {
	// envtest starts API server and etcd only; kube-controller-manager (which
	// runs the GC controller) is not started. OwnerReference cascade deletion
	// requires a real cluster.
	t.Skip("requires real cluster kube-controller-manager GC controller and WRAPPER-BL-ENVTEST-GC closed")

	ctx := context.Background()
	ns := "seam-system"

	// Create the PackExecution owner object manually (no reconciler needed).
	pe := &seamcorev1alpha1.InfrastructurePackExecution{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cascade-owner-pe",
			Namespace: ns,
		},
		Spec: seamcorev1alpha1.InfrastructurePackExecutionSpec{
			ClusterPackRef:      seamcorev1alpha1.InfrastructureClusterPackRef{Name: "some-pack", Version: "v1.0.0"},
			TargetClusterRef:    "ccs-test",
			AdmissionProfileRef: "some-profile",
		},
	}
	if err := k8sClient.Create(ctx, pe); err != nil {
		t.Fatalf("Create PackExecution: %v", err)
	}

	// Read back to get the UID for the ownerRef.
	pePatch := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pe.Name, Namespace: ns}, pePatch); err != nil {
		t.Fatalf("Get PackExecution UID: %v", err)
	}

	// Create the PackInstance with BlockOwnerDeletion ownerRef to PackExecution.
	// This mirrors the ownerRef set by PackExecutionReconciler in production.
	truePtr := true
	pi := &seamcorev1alpha1.InfrastructurePackInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cascade-owned-pi",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         seamcorev1alpha1.GroupVersion.String(),
				Kind:               "PackExecution",
				Name:               pePatch.Name,
				UID:                pePatch.UID,
				Controller:         &truePtr,
				BlockOwnerDeletion: &truePtr,
			}},
		},
		Spec: seamcorev1alpha1.InfrastructurePackInstanceSpec{
			ClusterPackRef:   "some-pack",
			TargetClusterRef: "ccs-test",
		},
	}
	if err := k8sClient.Create(ctx, pi); err != nil {
		t.Fatalf("Create PackInstance: %v", err)
	}

	// Confirm both objects exist.
	piNN := types.NamespacedName{Name: pi.Name, Namespace: ns}
	if err := k8sClient.Get(ctx, piNN, &seamcorev1alpha1.InfrastructurePackInstance{}); err != nil {
		t.Fatalf("PackInstance not found before cascade test: %v", err)
	}

	// Delete the PackExecution owner. The real API server GC controller should
	// cascade-delete the PackInstance once the PackExecution is gone.
	if err := k8sClient.Delete(ctx, pePatch); err != nil {
		t.Fatalf("Delete PackExecution: %v", err)
	}

	// Wait for the PackInstance to be cascade-deleted from etcd.
	// The Kubernetes GC controller runs in the background and may take a moment.
	ok := poll(t, 20*time.Second, func() bool {
		err := k8sClient.Get(ctx, piNN, &seamcorev1alpha1.InfrastructurePackInstance{})
		return apierrors.IsNotFound(err)
	})
	if !ok {
		t.Error("timed out: PackInstance was not cascade-deleted after PackExecution deletion")
	}
}
