package unit_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func newPackInstanceScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme clientgo: %v", err)
	}
	// Register only the seam-core types that the PackInstanceReconciler uses as typed
	// objects. InfrastructurePackReceipt is intentionally excluded: the reconciler
	// accesses it only via unstructured (getPackReceipt). Registering it as a typed
	// schema would cause the fake client to convert the unstructured receipt and strip
	// dynamic status fields (signatureVerified, driftStatus) that conductor writes but
	// are not yet declared in InfrastructurePackReceiptStatus.
	s.AddKnownTypes(seamcorev1alpha1.GroupVersion,
		&seamcorev1alpha1.InfrastructurePackInstance{},
		&seamcorev1alpha1.InfrastructurePackInstanceList{},
		&seamcorev1alpha1.InfrastructurePackExecution{},
		&seamcorev1alpha1.InfrastructurePackExecutionList{},
		&seamcorev1alpha1.InfrastructureClusterPack{},
		&seamcorev1alpha1.InfrastructureClusterPackList{},
	)
	return s
}

func newPackInstance(name, namespace, packRef, clusterRef string) *seamcorev1alpha1.InfrastructurePackInstance {
	return &seamcorev1alpha1.InfrastructurePackInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: seamcorev1alpha1.InfrastructurePackInstanceSpec{
			ClusterPackRef:   packRef,
			TargetClusterRef: clusterRef,
		},
	}
}

func newPackReceipt(name, namespace string, signatureVerified bool, driftStatus string) *unstructured.Unstructured {
	r := &unstructured.Unstructured{}
	r.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.ontai.dev",
		Version: "v1alpha1",
		Kind:    "InfrastructurePackReceipt",
	})
	r.SetName(name)
	r.SetNamespace(namespace)
	_ = unstructured.SetNestedField(r.Object, signatureVerified, "status", "signatureVerified")
	_ = unstructured.SetNestedField(r.Object, driftStatus, "status", "driftStatus")
	if driftStatus == "Drifted" {
		_ = unstructured.SetNestedField(r.Object, "2 resources missing", "status", "driftSummary")
	}
	return r
}

func reconcilePI(t *testing.T, r *controller.PackInstanceReconciler, pi *seamcorev1alpha1.InfrastructurePackInstance) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: pi.Name, Namespace: pi.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	return result
}

// TestPackInstanceReconciler_NoReceipt verifies that when no PackReceipt exists,
// the reconciler sets Progressing and requeueAfter=60s.
func TestPackInstanceReconciler_NoReceipt(t *testing.T) {
	s := newPackInstanceScheme(t)
	pi := newPackInstance("pi-1", "infra-system", "my-pack", "cluster-a")
	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackInstance{}).
		Build()
	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePI(t, r, pi)

	if result.RequeueAfter != 60*1e9 {
		t.Errorf("expected RequeueAfter=60s when receipt missing, got %v", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	progressingCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceProgressing)
	if progressingCond == nil || progressingCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Progressing=True when receipt missing")
	}
}

// TestPackInstanceReconciler_SecurityViolation verifies that signatureVerified=false
// in the PackReceipt raises SecurityViolation and sets Ready=False.
func TestPackInstanceReconciler_SecurityViolation(t *testing.T) {
	s := newPackInstanceScheme(t)
	pi := newPackInstance("pi-secviol", "infra-system", "my-pack", "cluster-a")
	receipt := newPackReceipt("my-pack-cluster-a", "infra-system", false, "InSync")
	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackInstance{}).
		Build()
	if err := fakeClient.Create(context.Background(), receipt); err != nil {
		t.Fatalf("create PackReceipt: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &seamcorev1alpha1.InfrastructurePackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	svCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceSecurityViolation)
	if svCond == nil {
		t.Fatal("expected SecurityViolation condition")
	}
	if svCond.Status != metav1.ConditionTrue {
		t.Errorf("expected SecurityViolation=True, got %v", svCond.Status)
	}
	readyCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False on security violation")
	}
}

// TestPackInstanceReconciler_DriftDetected verifies that driftStatus=Drifted in
// the PackReceipt raises Drifted=True and Ready=False.
func TestPackInstanceReconciler_DriftDetected(t *testing.T) {
	s := newPackInstanceScheme(t)
	pi := newPackInstance("pi-drift", "infra-system", "my-pack", "cluster-a")
	receipt := newPackReceipt("my-pack-cluster-a", "infra-system", true, "Drifted")
	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackInstance{}).
		Build()
	if err := fakeClient.Create(context.Background(), receipt); err != nil {
		t.Fatalf("create PackReceipt: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &seamcorev1alpha1.InfrastructurePackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	driftCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceDrifted)
	if driftCond == nil || driftCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Drifted=True")
	}
	readyCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False when drifted")
	}
}

// TestPackInstanceReconciler_InSync verifies that when signatureVerified=true and
// driftStatus=InSync, the PackInstance becomes Ready=True.
func TestPackInstanceReconciler_InSync(t *testing.T) {
	s := newPackInstanceScheme(t)
	pi := newPackInstance("pi-ok", "infra-system", "my-pack", "cluster-a")
	receipt := newPackReceipt("my-pack-cluster-a", "infra-system", true, "InSync")
	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackInstance{}).
		Build()
	if err := fakeClient.Create(context.Background(), receipt); err != nil {
		t.Fatalf("create PackReceipt: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &seamcorev1alpha1.InfrastructurePackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	readyCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True when in sync")
	}
	svCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceSecurityViolation)
	if svCond == nil || svCond.Status != metav1.ConditionFalse {
		t.Errorf("expected SecurityViolation=False when signature verified")
	}
}

// TestPackInstanceReconciler_DependencyBlock verifies that DriftPolicy=Block and a
// drifted dependency sets DependencyBlocked=True.
func TestPackInstanceReconciler_DependencyBlock(t *testing.T) {
	s := newPackInstanceScheme(t)

	// Dependency pack instance — drifted.
	depPI := newPackInstance("dep-pi", "infra-system", "dep-pack", "cluster-a")
	depPI.Status.Conditions = []metav1.Condition{
		{
			Type:               conditions.ConditionTypePackInstanceDrifted,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonDriftDetected,
			LastTransitionTime: metav1.Now(),
		},
	}

	pi := newPackInstance("pi-blocked", "infra-system", "my-pack", "cluster-a")
	pi.Spec.DependsOn = []string{"dep-pi"}
	pi.Spec.DependencyPolicy = &seamcorev1alpha1.InfrastructureDependencyPolicy{
		OnDrift: seamcorev1alpha1.InfrastructureDriftPolicyBlock,
	}

	receipt := newPackReceipt("my-pack-cluster-a", "infra-system", true, "InSync")

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi, depPI).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackInstance{}).
		Build()
	if err := fakeClient.Create(context.Background(), receipt); err != nil {
		t.Fatalf("create PackReceipt: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &seamcorev1alpha1.InfrastructurePackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	depBlockedCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceDependencyBlocked)
	if depBlockedCond == nil || depBlockedCond.Status != metav1.ConditionTrue {
		t.Errorf("expected DependencyBlocked=True when dependency is drifted and policy=Block")
	}
	readyCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False when dependency blocked")
	}
}

// newSucceededPackExecution builds a PackExecution with Succeeded=True for use in tests.
func newSucceededPackExecution(name, namespace, packName, packVersion, clusterRef string) *seamcorev1alpha1.InfrastructurePackExecution {
	pe := &seamcorev1alpha1.InfrastructurePackExecution{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: seamcorev1alpha1.InfrastructurePackExecutionSpec{
			ClusterPackRef: seamcorev1alpha1.InfrastructureClusterPackRef{
				Name:    packName,
				Version: packVersion,
			},
			TargetClusterRef:    clusterRef,
			AdmissionProfileRef: "rbac-profile",
		},
		Status: seamcorev1alpha1.InfrastructurePackExecutionStatus{
			Conditions: []metav1.Condition{
				{
					Type:               conditions.ConditionTypePackExecutionSucceeded,
					Status:             metav1.ConditionTrue,
					Reason:             conditions.ReasonJobSucceeded,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
	return pe
}

// TestPackInstanceReconciler_ReadyWhenPackExecutionSucceeded verifies that when a
// PackExecution for the matching pack+cluster has Succeeded=True, the PackInstance
// is set to Ready=True with reason PackDelivered — even when no PackReceipt exists.
// This is the trigger for DSNSReconciler in seam-core to emit the pack DNS TXT record.
func TestPackInstanceReconciler_ReadyWhenPackExecutionSucceeded(t *testing.T) {
	s := newPackInstanceScheme(t)
	pi := newPackInstance("pi-pe-ready", "infra-system", "my-pack", "cluster-a")
	pe := newSucceededPackExecution("pe-1", "infra-system", "my-pack", "v1.2.3", "cluster-a")

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackInstance{}, &seamcorev1alpha1.InfrastructurePackExecution{}).
		Build()
	// Create the PE with status via status subresource.
	if err := fakeClient.Create(context.Background(), pe); err != nil {
		t.Fatalf("create PackExecution: %v", err)
	}
	if err := fakeClient.Status().Update(context.Background(), pe); err != nil {
		t.Fatalf("update PackExecution status: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePI(t, r, pi)
	if result.RequeueAfter != 60*1e9 {
		t.Errorf("expected RequeueAfter=60s, got %v", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	readyCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceReady)
	if readyCond == nil {
		t.Fatal("expected Ready condition on PackInstance")
	}
	if readyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True when PackExecution Succeeded=True, got %v", readyCond.Status)
	}
	if readyCond.Reason != conditions.ReasonPackDelivered {
		t.Errorf("expected reason PackDelivered, got %q", readyCond.Reason)
	}
	wantMsg := "Pack my-pack v1.2.3 successfully delivered to cluster-a." // version already has v prefix
	if readyCond.Message != wantMsg {
		t.Errorf("Ready message mismatch\ngot:  %q\nwant: %q", readyCond.Message, wantMsg)
	}
}

// TestPackInstanceReconciler_AwaitingDeliveryWhenNoPackExecution verifies that when
// no PackExecution exists for the pack+cluster, Ready=False with AwaitingDelivery is set.
func TestPackInstanceReconciler_AwaitingDeliveryWhenNoPackExecution(t *testing.T) {
	s := newPackInstanceScheme(t)
	// No PackExecution and no PackReceipt — pack not yet deployed.
	pi := newPackInstance("pi-awaiting", "infra-system", "my-pack", "cluster-a")
	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackInstance{}).
		Build()

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &seamcorev1alpha1.InfrastructurePackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	readyCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackInstanceReady)
	if readyCond != nil && readyCond.Status == metav1.ConditionTrue {
		t.Error("expected Ready != True when no PackExecution exists")
	}
}

// TestPackInstanceReconciler_LineageSyncedInitialized verifies LineageSynced is
// set on first observation.
func TestPackInstanceReconciler_LineageSyncedInitialized(t *testing.T) {
	s := newPackInstanceScheme(t)
	pi := newPackInstance("pi-lineage", "infra-system", "my-pack", "cluster-a")
	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackInstance{}).
		Build()
	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &seamcorev1alpha1.InfrastructurePackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	lineageCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeLineageSynced)
	if lineageCond == nil {
		t.Fatal("expected LineageSynced condition initialized on first reconcile")
	}
	if lineageCond.Status != metav1.ConditionFalse {
		t.Errorf("expected LineageSynced=False, got %v", lineageCond.Status)
	}
}
