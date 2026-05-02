package unit_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clientevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/seam-core/pkg/conditions"
	"github.com/ontai-dev/wrapper/internal/controller"
)

func newClusterPackScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme clientgo: %v", err)
	}
	if err := seamcorev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme seamcorev1alpha1: %v", err)
	}
	return s
}

func newClusterPack(name, namespace, version string) *seamcorev1alpha1.InfrastructureClusterPack {
	return &seamcorev1alpha1.InfrastructureClusterPack{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Finalizers: []string{"infrastructure.ontai.dev/clusterpack-cleanup"},
		},
		Spec: seamcorev1alpha1.InfrastructureClusterPackSpec{
			Version: version,
			RegistryRef: seamcorev1alpha1.InfrastructurePackRegistryRef{
				URL:    "registry.ontai.dev/packs/" + name,
				Digest: "sha256:abc123",
			},
			Checksum: "sha256:def456",
		},
	}
}

func reconcileCP(t *testing.T, r *controller.ClusterPackReconciler, cp *seamcorev1alpha1.InfrastructureClusterPack) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	return result
}

// TestClusterPackReconciler_AwaitingSignature verifies that a newly created
// ClusterPack without a signature annotation enters the SignaturePending state
// and requeueAfter=15s.
func TestClusterPackReconciler_AwaitingSignature(t *testing.T) {
	s := newClusterPackScheme(t)
	cp := newClusterPack("my-pack", "infra-system", "v1.0.0")
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructureClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcileCP(t, r, cp)

	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s, got %v", result.RequeueAfter)
	}

	// Re-fetch to check status.
	updated := &seamcorev1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cp), updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}

	sigPendingCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeClusterPackSignaturePending)
	if sigPendingCond == nil {
		t.Fatal("expected SignaturePending condition to be set")
	}
	if sigPendingCond.Status != metav1.ConditionTrue {
		t.Errorf("expected SignaturePending=True, got %v", sigPendingCond.Status)
	}

	availCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeClusterPackAvailable)
	if availCond == nil {
		t.Fatal("expected Available condition to be set")
	}
	if availCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Available=False, got %v", availCond.Status)
	}
}

// TestClusterPackReconciler_SignedTransitionsToAvailable verifies that when the
// conductor signature annotation is present, the ClusterPack transitions to
// Available and status.Signed=true.
func TestClusterPackReconciler_SignedTransitionsToAvailable(t *testing.T) {
	s := newClusterPackScheme(t)
	cp := newClusterPack("signed-pack", "infra-system", "v1.0.0")
	cp.Annotations = map[string]string{
		"ontai.dev/pack-signature":           "base64sig==",
		"infrastructure.ontai.dev/spec-checksum-snapshot": cp.Spec.Checksum + "|" + cp.Spec.RegistryRef.URL + "|" + cp.Spec.RegistryRef.Digest + "|" + cp.Spec.Version,
	}
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructureClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcileCP(t, r, cp)

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue on signed pack, got %+v", result)
	}

	updated := &seamcorev1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cp), updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}

	if !updated.Status.Signed {
		t.Error("expected status.Signed=true")
	}
	if updated.Status.PackSignature != "base64sig==" {
		t.Errorf("expected PackSignature=base64sig==, got %q", updated.Status.PackSignature)
	}

	availCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeClusterPackAvailable)
	if availCond == nil {
		t.Fatal("expected Available condition")
	}
	if availCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Available=True, got %v", availCond.Status)
	}
}

// TestClusterPackReconciler_LineageSyncedInitialized verifies that LineageSynced
// is set to False/LineageControllerAbsent on first reconcile.
func TestClusterPackReconciler_LineageSyncedInitialized(t *testing.T) {
	s := newClusterPackScheme(t)
	cp := newClusterPack("lineage-pack", "infra-system", "v1.0.0")
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructureClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	reconcileCP(t, r, cp)

	updated := &seamcorev1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cp), updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}

	lineageCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeLineageSynced)
	if lineageCond == nil {
		t.Fatal("expected LineageSynced condition")
	}
	if lineageCond.Status != metav1.ConditionFalse {
		t.Errorf("expected LineageSynced=False, got %v", lineageCond.Status)
	}
	if lineageCond.Reason != conditions.ReasonLineageControllerAbsent {
		t.Errorf("expected reason %q, got %q", conditions.ReasonLineageControllerAbsent, lineageCond.Reason)
	}
}

// TestClusterPackReconciler_ImmutabilityViolation verifies that a spec mutation
// after the snapshot is recorded triggers ImmutabilityViolation.
func TestClusterPackReconciler_ImmutabilityViolation(t *testing.T) {
	s := newClusterPackScheme(t)
	cp := newClusterPack("immutable-pack", "infra-system", "v1.0.0")
	// Pre-set the snapshot annotation with a different checksum to simulate mutation.
	cp.Annotations = map[string]string{
		"infrastructure.ontai.dev/spec-checksum-snapshot": "sha256:different|old-url|old-digest|v0.9.0",
	}
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructureClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcileCP(t, r, cp)

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue on immutability violation, got %+v", result)
	}

	updated := &seamcorev1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cp), updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}

	immCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeClusterPackImmutabilityViolation)
	if immCond == nil {
		t.Fatal("expected ImmutabilityViolation condition")
	}
	if immCond.Status != metav1.ConditionTrue {
		t.Errorf("expected ImmutabilityViolation=True, got %v", immCond.Status)
	}
}

// TestClusterPackReconciler_DeletionCascadesDriftSignal verifies that handleClusterPackDeletion
// deletes the DriftSignal "drift-{cp.Name}" from "seam-tenant-{clusterName}" for each
// target cluster, alongside PackInstances and PackExecutions.
func TestClusterPackReconciler_DeletionCascadesDriftSignal(t *testing.T) {
	s := newClusterPackScheme(t)

	clusterName := "ccs-dev"
	tenantNS := "seam-tenant-" + clusterName
	cpName := "nginx-pack"

	cp := newClusterPack(cpName, "infra-system", "v1.0.0")
	cp.Spec.TargetClusters = []string{clusterName}
	now := metav1.Now()
	cp.DeletionTimestamp = &now

	signal := &seamcorev1alpha1.DriftSignal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "drift-" + cpName,
			Namespace: tenantNS,
		},
		Spec: seamcorev1alpha1.DriftSignalSpec{
			State: seamcorev1alpha1.DriftSignalStatePending,
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, signal).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructureClusterPack{}).
		Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: cp.Name, Namespace: cp.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}

	remaining := &seamcorev1alpha1.DriftSignal{}
	getErr := fakeClient.Get(context.Background(),
		client.ObjectKey{Name: "drift-" + cpName, Namespace: tenantNS}, remaining)
	if !apierrors.IsNotFound(getErr) {
		t.Errorf("expected DriftSignal to be deleted, got: %v", getErr)
	}
}

// TestClusterPackReconciler_RevokedNoRequeue verifies that a revoked ClusterPack
// stops reconciliation without requeue.
func TestClusterPackReconciler_RevokedNoRequeue(t *testing.T) {
	s := newClusterPackScheme(t)
	cp := newClusterPack("revoked-pack", "infra-system", "v1.0.0")
	// Pre-set revoked condition and snapshot.
	cp.Annotations = map[string]string{
		"infrastructure.ontai.dev/spec-checksum-snapshot": cp.Spec.Checksum + "|" + cp.Spec.RegistryRef.URL + "|" + cp.Spec.RegistryRef.Digest + "|" + cp.Spec.Version,
	}
	cp.Status.Conditions = []metav1.Condition{
		{
			Type:               conditions.ConditionTypeClusterPackRevoked,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonPackRevoked,
			Message:            "revoked",
			LastTransitionTime: metav1.Now(),
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructureClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcileCP(t, r, cp)

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for revoked pack, got %+v", result)
	}
}
