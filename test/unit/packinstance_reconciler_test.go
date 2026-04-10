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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

func newPackInstanceScheme(t *testing.T) *runtime.Scheme {
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

func newPackInstance(name, namespace, packRef, clusterRef string) *infrav1alpha1.PackInstance {
	return &infrav1alpha1.PackInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: infrav1alpha1.PackInstanceSpec{
			ClusterPackRef:   packRef,
			TargetClusterRef: clusterRef,
		},
	}
}

func newPackReceipt(name, namespace string, signatureVerified bool, driftStatus string) *unstructured.Unstructured {
	r := &unstructured.Unstructured{}
	r.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infra.ontai.dev",
		Version: "v1alpha1",
		Kind:    "PackReceipt",
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

func reconcilePI(t *testing.T, r *controller.PackInstanceReconciler, pi *infrav1alpha1.PackInstance) ctrl.Result {
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
		WithStatusSubresource(&infrav1alpha1.PackInstance{}).
		Build()
	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	result := reconcilePI(t, r, pi)

	if result.RequeueAfter != 60*1e9 {
		t.Errorf("expected RequeueAfter=60s when receipt missing, got %v", result.RequeueAfter)
	}

	updated := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	progressingCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceProgressing)
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
		WithStatusSubresource(&infrav1alpha1.PackInstance{}).
		Build()
	if err := fakeClient.Create(context.Background(), receipt); err != nil {
		t.Fatalf("create PackReceipt: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	svCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceSecurityViolation)
	if svCond == nil {
		t.Fatal("expected SecurityViolation condition")
	}
	if svCond.Status != metav1.ConditionTrue {
		t.Errorf("expected SecurityViolation=True, got %v", svCond.Status)
	}
	readyCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceReady)
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
		WithStatusSubresource(&infrav1alpha1.PackInstance{}).
		Build()
	if err := fakeClient.Create(context.Background(), receipt); err != nil {
		t.Fatalf("create PackReceipt: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	driftCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceDrifted)
	if driftCond == nil || driftCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Drifted=True")
	}
	readyCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceReady)
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
		WithStatusSubresource(&infrav1alpha1.PackInstance{}).
		Build()
	if err := fakeClient.Create(context.Background(), receipt); err != nil {
		t.Fatalf("create PackReceipt: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	readyCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True when in sync")
	}
	svCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceSecurityViolation)
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
			Type:               infrav1alpha1.ConditionTypePackInstanceDrifted,
			Status:             metav1.ConditionTrue,
			Reason:             infrav1alpha1.ReasonDriftDetected,
			LastTransitionTime: metav1.Now(),
		},
	}

	pi := newPackInstance("pi-blocked", "infra-system", "my-pack", "cluster-a")
	pi.Spec.DependsOn = []string{"dep-pi"}
	pi.Spec.DependencyPolicy = &infrav1alpha1.DependencyPolicy{
		OnDrift: infrav1alpha1.DriftPolicyBlock,
	}

	receipt := newPackReceipt("my-pack-cluster-a", "infra-system", true, "InSync")

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi, depPI).
		WithStatusSubresource(&infrav1alpha1.PackInstance{}).
		Build()
	if err := fakeClient.Create(context.Background(), receipt); err != nil {
		t.Fatalf("create PackReceipt: %v", err)
	}

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	depBlockedCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceDependencyBlocked)
	if depBlockedCond == nil || depBlockedCond.Status != metav1.ConditionTrue {
		t.Errorf("expected DependencyBlocked=True when dependency is drifted and policy=Block")
	}
	readyCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceReady)
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False when dependency blocked")
	}
}

// newSucceededPackExecution builds a PackExecution with Succeeded=True for use in tests.
func newSucceededPackExecution(name, namespace, packName, packVersion, clusterRef string) *infrav1alpha1.PackExecution {
	pe := &infrav1alpha1.PackExecution{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: infrav1alpha1.PackExecutionSpec{
			ClusterPackRef: infrav1alpha1.ClusterPackRef{
				Name:    packName,
				Version: packVersion,
			},
			TargetClusterRef:    clusterRef,
			AdmissionProfileRef: "rbac-profile",
		},
		Status: infrav1alpha1.PackExecutionStatus{
			Conditions: []metav1.Condition{
				{
					Type:               infrav1alpha1.ConditionTypePackExecutionSucceeded,
					Status:             metav1.ConditionTrue,
					Reason:             infrav1alpha1.ReasonJobSucceeded,
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
	pe := newSucceededPackExecution("pe-1", "infra-system", "my-pack", "1.2.3", "cluster-a")

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(pi).
		WithStatusSubresource(&infrav1alpha1.PackInstance{}, &infrav1alpha1.PackExecution{}).
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
		Recorder: record.NewFakeRecorder(10),
	}

	result := reconcilePI(t, r, pi)
	if result.RequeueAfter != 60*1e9 {
		t.Errorf("expected RequeueAfter=60s, got %v", result.RequeueAfter)
	}

	updated := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	readyCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceReady)
	if readyCond == nil {
		t.Fatal("expected Ready condition on PackInstance")
	}
	if readyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready=True when PackExecution Succeeded=True, got %v", readyCond.Status)
	}
	if readyCond.Reason != infrav1alpha1.ReasonPackDelivered {
		t.Errorf("expected reason PackDelivered, got %q", readyCond.Reason)
	}
	wantMsg := "Pack my-pack v1.2.3 successfully delivered to cluster-a."
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
		WithStatusSubresource(&infrav1alpha1.PackInstance{}).
		Build()

	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	readyCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceReady)
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
		WithStatusSubresource(&infrav1alpha1.PackInstance{}).
		Build()
	r := &controller.PackInstanceReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(10),
	}

	reconcilePI(t, r, pi)

	updated := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pi), updated); err != nil {
		t.Fatalf("get updated PackInstance: %v", err)
	}
	lineageCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeLineageSynced)
	if lineageCond == nil {
		t.Fatal("expected LineageSynced condition initialized on first reconcile")
	}
	if lineageCond.Status != metav1.ConditionFalse {
		t.Errorf("expected LineageSynced=False, got %v", lineageCond.Status)
	}
}
