package unit_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

func newClusterPackScheme(t *testing.T) *runtime.Scheme {
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

func newClusterPack(name, namespace, version string) *infrav1alpha1.ClusterPack {
	return &infrav1alpha1.ClusterPack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: infrav1alpha1.ClusterPackSpec{
			Version: version,
			RegistryRef: infrav1alpha1.PackRegistryRef{
				URL:    "registry.ontai.dev/packs/" + name,
				Digest: "sha256:abc123",
			},
			Checksum: "sha256:def456",
		},
	}
}

func reconcileCP(t *testing.T, r *controller.ClusterPackReconciler, cp *infrav1alpha1.ClusterPack) ctrl.Result {
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
		WithStatusSubresource(&infrav1alpha1.ClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result := reconcileCP(t, r, cp)

	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s, got %v", result.RequeueAfter)
	}

	// Re-fetch to check status.
	updated := &infrav1alpha1.ClusterPack{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cp), updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}

	sigPendingCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeClusterPackSignaturePending)
	if sigPendingCond == nil {
		t.Fatal("expected SignaturePending condition to be set")
	}
	if sigPendingCond.Status != metav1.ConditionTrue {
		t.Errorf("expected SignaturePending=True, got %v", sigPendingCond.Status)
	}

	availCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeClusterPackAvailable)
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
		"infra.ontai.dev/spec-checksum-snapshot": cp.Spec.Checksum + "|" + cp.Spec.RegistryRef.URL + "|" + cp.Spec.RegistryRef.Digest + "|" + cp.Spec.Version,
	}
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&infrav1alpha1.ClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result := reconcileCP(t, r, cp)

	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected no requeue on signed pack, got %+v", result)
	}

	updated := &infrav1alpha1.ClusterPack{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cp), updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}

	if !updated.Status.Signed {
		t.Error("expected status.Signed=true")
	}
	if updated.Status.PackSignature != "base64sig==" {
		t.Errorf("expected PackSignature=base64sig==, got %q", updated.Status.PackSignature)
	}

	availCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeClusterPackAvailable)
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
		WithStatusSubresource(&infrav1alpha1.ClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcileCP(t, r, cp)

	updated := &infrav1alpha1.ClusterPack{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cp), updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}

	lineageCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeLineageSynced)
	if lineageCond == nil {
		t.Fatal("expected LineageSynced condition")
	}
	if lineageCond.Status != metav1.ConditionFalse {
		t.Errorf("expected LineageSynced=False, got %v", lineageCond.Status)
	}
	if lineageCond.Reason != infrav1alpha1.ReasonLineageControllerAbsent {
		t.Errorf("expected reason %q, got %q", infrav1alpha1.ReasonLineageControllerAbsent, lineageCond.Reason)
	}
}

// TestClusterPackReconciler_ImmutabilityViolation verifies that a spec mutation
// after the snapshot is recorded triggers ImmutabilityViolation.
func TestClusterPackReconciler_ImmutabilityViolation(t *testing.T) {
	s := newClusterPackScheme(t)
	cp := newClusterPack("immutable-pack", "infra-system", "v1.0.0")
	// Pre-set the snapshot annotation with a different checksum to simulate mutation.
	cp.Annotations = map[string]string{
		"infra.ontai.dev/spec-checksum-snapshot": "sha256:different|old-url|old-digest|v0.9.0",
	}
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&infrav1alpha1.ClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result := reconcileCP(t, r, cp)

	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected no requeue on immutability violation, got %+v", result)
	}

	updated := &infrav1alpha1.ClusterPack{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(cp), updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}

	immCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeClusterPackImmutabilityViolation)
	if immCond == nil {
		t.Fatal("expected ImmutabilityViolation condition")
	}
	if immCond.Status != metav1.ConditionTrue {
		t.Errorf("expected ImmutabilityViolation=True, got %v", immCond.Status)
	}
}

// TestClusterPackReconciler_RevokedNoRequeue verifies that a revoked ClusterPack
// stops reconciliation without requeue.
func TestClusterPackReconciler_RevokedNoRequeue(t *testing.T) {
	s := newClusterPackScheme(t)
	cp := newClusterPack("revoked-pack", "infra-system", "v1.0.0")
	// Pre-set revoked condition and snapshot.
	cp.Annotations = map[string]string{
		"infra.ontai.dev/spec-checksum-snapshot": cp.Spec.Checksum + "|" + cp.Spec.RegistryRef.URL + "|" + cp.Spec.RegistryRef.Digest + "|" + cp.Spec.Version,
	}
	cp.Status.Conditions = []metav1.Condition{
		{
			Type:               infrav1alpha1.ConditionTypeClusterPackRevoked,
			Status:             metav1.ConditionTrue,
			Reason:             infrav1alpha1.ReasonPackRevoked,
			Message:            "revoked",
			LastTransitionTime: metav1.Now(),
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&infrav1alpha1.ClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result := reconcileCP(t, r, cp)

	if result.Requeue || result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for revoked pack, got %+v", result)
	}
}
