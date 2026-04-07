// Package controller_test — PackInstance lifecycle unit tests.
//
// Workstream 1: PackInstance lifecycle and ownership chain.
//
// Ownership chain: ClusterPack owns PackExecution owns PackInstance.
// PackReceipt is NOT created by wrapper — it is created by the Conductor agent
// on the tenant cluster after signature verification. wrapper-design.md §7, §8.
//
// Tests cover:
//   - ClusterPack + existing TalosCluster: PackExecution processed, PackInstance
//     created with correct ownerReference pointing to PackExecution.
//   - ClusterPack + absent TalosCluster: WaitingForCluster condition set, no Job,
//     no PackInstance created.
//   - Deletion path (INV-006): PackExecutionReconciler returns no-op when PE is
//     gone; ClusterPackReconciler never submits Jobs.
//   - All five gates independently block Job submission with the correct condition.
package controller_test

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

// reconcilePackExecution calls PackExecutionReconciler.Reconcile and fatals on error.
func reconcilePackExecution(t *testing.T, r *controller.PackExecutionReconciler, name, namespace string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
	})
	if err != nil {
		t.Fatalf("PackExecutionReconciler.Reconcile: %v", err)
	}
	return result
}

// allGatesSetup creates a fake client with all five gates satisfied:
//   - ClusterPack: signed (gate 1 clear, gate 2 clear)
//   - TalosCluster: ConductorReady=True (gate 0 clear)
//   - PermissionSnapshot: Current=True (gate 3 clear)
//   - RBACProfile: provisioned=true (gate 4 clear)
func allGatesSetup(t *testing.T, peName, cpName, cpVersion, clusterRef, profileRef string) (client.Client, *infrav1alpha1.PackExecution) {
	t.Helper()
	s := buildTestScheme(t)

	cp := newSignedCP(cpName, cpVersion, "infra-system")
	pe := newPE(peName, cpName, cpVersion, cp.UID, clusterRef, profileRef, "infra-system")
	tc := newTalosCluster(clusterRef, true)
	ps := newPermissionSnapshot(clusterRef, "infra-system", true)
	rp := newRBACProfile(profileRef, "infra-system", true)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.PackInstance{}).
		Build()

	ctx := context.Background()
	for _, obj := range []client.Object{tc, ps, rp} {
		if err := fakeClient.Create(ctx, obj); err != nil {
			t.Fatalf("allGatesSetup: create %T: %v", obj, err)
		}
	}
	return fakeClient, pe
}

// TestOwnershipChain_TalosClusterExists verifies the PackInstance lifecycle happy path.
//
// Setup: signed ClusterPack + PackExecution (ownerRef to ClusterPack) + TalosCluster
// (ConductorReady=True) + current PermissionSnapshot + provisioned RBACProfile +
// pack-deploy Job (Succeeded=1) + OperationResult ConfigMap.
//
// Assertions:
//  1. PackExecution has ownerRef to ClusterPack (ownership chain structure).
//  2. PackExecution transitions to Succeeded=True after reconcile.
//  3. PackInstance is created with correct ownerRef to PackExecution.
//  4. PackInstance.Spec.ClusterPackRef and TargetClusterRef are correct.
func TestOwnershipChain_TalosClusterExists(t *testing.T) {
	const (
		peName     = "pe-happy"
		cpName     = "my-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-a"
		profileRef = "profile-a"
	)

	fakeClient, pe := allGatesSetup(t, peName, cpName, cpVersion, clusterRef, profileRef)
	ctx := context.Background()

	// Add succeeded Job and OperationResult CM.
	job := newJob(packDeployJobName(peName), "infra-system", 1, 0)
	cm := newOperationResultCM(peName, "infra-system")
	if err := fakeClient.Create(ctx, job); err != nil {
		t.Fatalf("create Job: %v", err)
	}
	if err := fakeClient.Create(ctx, cm); err != nil {
		t.Fatalf("create OperationResult CM: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: record.NewFakeRecorder(32),
	}

	// Assertion 1: ownership chain structure — PE has ownerRef to CP.
	if len(pe.OwnerReferences) != 1 {
		t.Fatalf("PE must have exactly 1 ownerReference, got %d", len(pe.OwnerReferences))
	}
	ownerRef := pe.OwnerReferences[0]
	if ownerRef.Kind != "ClusterPack" || ownerRef.Name != cpName {
		t.Errorf("PE ownerRef Kind=%q Name=%q, want Kind=ClusterPack Name=%s",
			ownerRef.Kind, ownerRef.Name, cpName)
	}
	if !*ownerRef.Controller || !*ownerRef.BlockOwnerDeletion {
		t.Error("PE ownerRef must have Controller=true and BlockOwnerDeletion=true")
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")

	// Must return without requeue (terminal success state).
	if result.RequeueAfter != 0 || result.Requeue {
		t.Errorf("expected no requeue after success, got %+v", result)
	}

	// Assertion 2: PackExecution Succeeded=True.
	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	succeededCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionSucceeded)
	if succeededCond == nil {
		t.Fatal("PackExecution Succeeded condition not set after Job success")
	}
	if succeededCond.Status != metav1.ConditionTrue {
		t.Errorf("PackExecution Succeeded=%s, want True", succeededCond.Status)
	}
	if updated.Status.OperationResultRef != "pack-deploy-result-"+peName {
		t.Errorf("OperationResultRef=%q, want %q",
			updated.Status.OperationResultRef, "pack-deploy-result-"+peName)
	}

	// Assertion 3: PackInstance created with ownerRef to PackExecution.
	piName := cpName + "-" + clusterRef // "my-pack-cluster-a"
	pi := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: piName, Namespace: "infra-system"}, pi); err != nil {
		t.Fatalf("PackInstance %q not created after Job success: %v", piName, err)
	}
	if len(pi.OwnerReferences) != 1 {
		t.Fatalf("PackInstance must have exactly 1 ownerReference, got %d", len(pi.OwnerReferences))
	}
	piOwner := pi.OwnerReferences[0]
	if piOwner.Kind != "PackExecution" || piOwner.Name != peName {
		t.Errorf("PackInstance ownerRef Kind=%q Name=%q, want Kind=PackExecution Name=%s",
			piOwner.Kind, piOwner.Name, peName)
	}
	if piOwner.UID != pe.UID {
		t.Errorf("PackInstance ownerRef.UID=%q, want %q", piOwner.UID, pe.UID)
	}
	if !*piOwner.Controller || !*piOwner.BlockOwnerDeletion {
		t.Error("PackInstance ownerRef must have Controller=true and BlockOwnerDeletion=true")
	}

	// Assertion 4: PackInstance spec fields.
	if pi.Spec.ClusterPackRef != cpName {
		t.Errorf("PackInstance.Spec.ClusterPackRef=%q, want %q", pi.Spec.ClusterPackRef, cpName)
	}
	if pi.Spec.TargetClusterRef != clusterRef {
		t.Errorf("PackInstance.Spec.TargetClusterRef=%q, want %q", pi.Spec.TargetClusterRef, clusterRef)
	}
}

// TestWaitingForCluster_TalosClusterAbsent verifies that when the target TalosCluster
// does not exist, the PackExecutionReconciler sets Waiting=True with
// ReasonAwaitingConductorReady, requeues, and creates neither a Job nor a PackInstance.
func TestWaitingForCluster_TalosClusterAbsent(t *testing.T) {
	const (
		peName     = "pe-no-tc"
		cpName     = "my-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-missing"
		profileRef = "profile-a"
	)

	s := buildTestScheme(t)
	cp := newSignedCP(cpName, cpVersion, "infra-system")
	pe := newPE(peName, cpName, cpVersion, cp.UID, clusterRef, profileRef, "infra-system")

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}).
		Build()
	// TalosCluster for "cluster-missing" is deliberately absent.

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(32),
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")
	ctx := context.Background()

	// Must requeue — waiting for cluster to become available.
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter > 0 when TalosCluster is absent, got 0")
	}

	// Waiting=True with AwaitingConductorReady reason.
	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	waitCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionWaiting)
	if waitCond == nil {
		t.Fatal("Waiting condition not set when TalosCluster is absent")
	}
	if waitCond.Status != metav1.ConditionTrue {
		t.Errorf("Waiting=%s, want True", waitCond.Status)
	}
	if waitCond.Reason != infrav1alpha1.ReasonAwaitingConductorReady {
		t.Errorf("Waiting reason=%q, want %q", waitCond.Reason, infrav1alpha1.ReasonAwaitingConductorReady)
	}

	// No Jobs created — INV-006.
	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) > 0 {
		t.Errorf("expected no Jobs when TalosCluster is absent (INV-006), got %d", len(jobList.Items))
	}

	// No PackInstances created.
	piList := &infrav1alpha1.PackInstanceList{}
	if err := fakeClient.List(ctx, piList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list PackInstances: %v", err)
	}
	if len(piList.Items) > 0 {
		t.Errorf("expected no PackInstances when TalosCluster is absent, got %d", len(piList.Items))
	}
}

// TestDeletion_PackExecutionNotFound_NoJobCreated verifies INV-006:
// when a PackExecution is not found (already deleted), the reconciler returns
// without requeue and creates no Kueue Jobs. Deletion must never trigger Jobs.
func TestDeletion_PackExecutionNotFound_NoJobCreated(t *testing.T) {
	s := buildTestScheme(t)
	// Empty fake client — PE does not exist (already deleted).
	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(32),
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "deleted-pe", Namespace: "infra-system"},
	})
	if err != nil {
		t.Fatalf("unexpected error reconciling deleted PE: %v", err)
	}
	if result.RequeueAfter != 0 || result.Requeue {
		t.Errorf("expected no requeue for deleted PE, got %+v", result)
	}

	// No Jobs created on the deletion path. INV-006.
	ctx := context.Background()
	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) > 0 {
		t.Errorf("expected no Jobs when PE not found (deletion path, INV-006), got %d", len(jobList.Items))
	}
}

// TestDeletion_ClusterPackReconciler_NoJobsSubmitted verifies that the
// ClusterPackReconciler never submits Kueue Jobs under any circumstance.
// INV-006: deletion triggers events only. wrapper-design.md §3.
func TestDeletion_ClusterPackReconciler_NoJobsSubmitted(t *testing.T) {
	s := buildTestScheme(t)
	cp := &infrav1alpha1.ClusterPack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pack",
			Namespace: "infra-system",
		},
		Spec: infrav1alpha1.ClusterPackSpec{
			Version: "v1.0.0",
			RegistryRef: infrav1alpha1.PackRegistryRef{
				URL:    "registry.ontai.dev/packs/test-pack",
				Digest: "sha256:abc123",
			},
			Checksum: "sha256:def456",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp).
		WithStatusSubresource(&infrav1alpha1.ClusterPack{}).
		Build()

	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(32),
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-pack", Namespace: "infra-system"},
	})
	if err != nil {
		t.Fatalf("ClusterPackReconciler.Reconcile: %v", err)
	}

	// ClusterPackReconciler must never create Jobs. wrapper-design.md §3. INV-006.
	ctx := context.Background()
	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) > 0 {
		t.Errorf("ClusterPackReconciler must not create Jobs (INV-006), got %d Job(s)", len(jobList.Items))
	}
}

// TestGate0_ConductorReadyFalse verifies that Gate 0 blocks Job submission and
// surfaces Waiting=True with ReasonAwaitingConductorReady when the target
// TalosCluster has ConductorReady=False. platform-schema.md §12, Gap 27.
func TestGate0_ConductorReadyFalse(t *testing.T) {
	const (
		peName     = "pe-gate0"
		cpName     = "my-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-b"
		profileRef = "profile-b"
	)

	s := buildTestScheme(t)
	cp := newSignedCP(cpName, cpVersion, "infra-system")
	pe := newPE(peName, cpName, cpVersion, cp.UID, clusterRef, profileRef, "infra-system")
	tc := newTalosCluster(clusterRef, false) // ConductorReady=False

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}).
		Build()
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(32),
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")

	if result.RequeueAfter == 0 {
		t.Error("expected requeue when Gate 0 (ConductorReady=False) not cleared")
	}

	ctx := context.Background()
	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}

	waitCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionWaiting)
	if waitCond == nil || waitCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Waiting=True when ConductorReady=False, got %+v", waitCond)
	}
	if waitCond.Reason != infrav1alpha1.ReasonAwaitingConductorReady {
		t.Errorf("Waiting reason=%q, want %q", waitCond.Reason, infrav1alpha1.ReasonAwaitingConductorReady)
	}

	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) > 0 {
		t.Errorf("no Job expected when Gate 0 blocked, got %d", len(jobList.Items))
	}
}

// TestGate1_SignaturePending verifies that Gate 1 (ClusterPack.status.Signed=false)
// blocks Job submission with PackSignaturePending=True and 15s requeue.
func TestGate1_SignaturePending(t *testing.T) {
	const (
		peName     = "pe-gate1"
		cpName     = "unsigned-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-c"
		profileRef = "profile-c"
	)

	s := buildTestScheme(t)
	cp := newSignedCP(cpName, cpVersion, "infra-system")
	cp.Status.Signed = false // Override: not yet signed.
	pe := newPE(peName, cpName, cpVersion, cp.UID, clusterRef, profileRef, "infra-system")
	tc := newTalosCluster(clusterRef, true)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}).
		Build()
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(32),
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")

	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s for signature gate, got %v", result.RequeueAfter)
	}

	ctx := context.Background()
	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}

	sigPendingCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackSignaturePending)
	if sigPendingCond == nil || sigPendingCond.Status != metav1.ConditionTrue {
		t.Errorf("expected PackSignaturePending=True when unsigned, got %+v", sigPendingCond)
	}
	if sigPendingCond.Reason != infrav1alpha1.ReasonAwaitingSignature {
		t.Errorf("reason=%q, want %q", sigPendingCond.Reason, infrav1alpha1.ReasonAwaitingSignature)
	}

	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) > 0 {
		t.Errorf("no Job expected when Gate 1 blocked, got %d", len(jobList.Items))
	}
}

// TestGate2_PackRevoked verifies that Gate 2 (ClusterPack Revoked=True) blocks
// Job submission with PackRevoked=True and no requeue (human intervention required).
func TestGate2_PackRevoked(t *testing.T) {
	const (
		peName     = "pe-gate2"
		cpName     = "revoked-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-d"
		profileRef = "profile-d"
	)

	s := buildTestScheme(t)
	cp := newSignedCP(cpName, cpVersion, "infra-system")
	cp.Status.Conditions = []metav1.Condition{{
		Type:               infrav1alpha1.ConditionTypeClusterPackRevoked,
		Status:             metav1.ConditionTrue,
		Reason:             infrav1alpha1.ReasonPackRevoked,
		Message:            "revoked by platform governor",
		LastTransitionTime: metav1.Now(),
	}}
	pe := newPE(peName, cpName, cpVersion, cp.UID, clusterRef, profileRef, "infra-system")
	tc := newTalosCluster(clusterRef, true)
	ps := newPermissionSnapshot(clusterRef, "infra-system", true)
	rp := newRBACProfile(profileRef, "infra-system", true)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}).
		Build()
	ctx := context.Background()
	for _, obj := range []client.Object{tc, ps, rp} {
		if err := fakeClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(32),
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")

	// No requeue — revocation requires human intervention.
	if result.RequeueAfter != 0 || result.Requeue {
		t.Errorf("expected no requeue when pack revoked (human intervention), got %+v", result)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	revokedCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackRevoked)
	if revokedCond == nil || revokedCond.Status != metav1.ConditionTrue {
		t.Errorf("expected PackRevoked=True, got %+v", revokedCond)
	}

	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) > 0 {
		t.Errorf("no Job expected when ClusterPack revoked, got %d", len(jobList.Items))
	}
}

// TestGate3_PermissionSnapshotOutOfSync verifies that Gate 3 blocks Job submission
// when the PermissionSnapshot for the target cluster is not current.
func TestGate3_PermissionSnapshotOutOfSync(t *testing.T) {
	const (
		peName     = "pe-gate3"
		cpName     = "my-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-e"
		profileRef = "profile-e"
	)

	s := buildTestScheme(t)
	cp := newSignedCP(cpName, cpVersion, "infra-system")
	pe := newPE(peName, cpName, cpVersion, cp.UID, clusterRef, profileRef, "infra-system")
	tc := newTalosCluster(clusterRef, true)
	ps := newPermissionSnapshot(clusterRef, "infra-system", false) // current=false
	rp := newRBACProfile(profileRef, "infra-system", true)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}).
		Build()
	ctx := context.Background()
	for _, obj := range []client.Object{tc, ps, rp} {
		if err := fakeClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(32),
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")

	if result.RequeueAfter == 0 {
		t.Error("expected requeue when Gate 3 (PermissionSnapshot) not cleared")
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	psCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePermissionSnapshotOutOfSync)
	if psCond == nil || psCond.Status != metav1.ConditionTrue {
		t.Errorf("expected PermissionSnapshotOutOfSync=True, got %+v", psCond)
	}
	if psCond.Reason != infrav1alpha1.ReasonSnapshotOutOfSync {
		t.Errorf("reason=%q, want %q", psCond.Reason, infrav1alpha1.ReasonSnapshotOutOfSync)
	}

	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) > 0 {
		t.Errorf("no Job expected when Gate 3 blocked, got %d", len(jobList.Items))
	}
}

// TestGate4_RBACProfileNotProvisioned verifies that Gate 4 blocks Job submission
// when the RBACProfile for the admissionProfileRef is not yet provisioned.
func TestGate4_RBACProfileNotProvisioned(t *testing.T) {
	const (
		peName     = "pe-gate4"
		cpName     = "my-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-f"
		profileRef = "profile-f"
	)

	s := buildTestScheme(t)
	cp := newSignedCP(cpName, cpVersion, "infra-system")
	pe := newPE(peName, cpName, cpVersion, cp.UID, clusterRef, profileRef, "infra-system")
	tc := newTalosCluster(clusterRef, true)
	ps := newPermissionSnapshot(clusterRef, "infra-system", true)
	rp := newRBACProfile(profileRef, "infra-system", false) // provisioned=false

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}).
		Build()
	ctx := context.Background()
	for _, obj := range []client.Object{tc, ps, rp} {
		if err := fakeClient.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(32),
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")

	if result.RequeueAfter == 0 {
		t.Error("expected requeue when Gate 4 (RBACProfile) not cleared")
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	rbacCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeRBACProfileNotProvisioned)
	if rbacCond == nil || rbacCond.Status != metav1.ConditionTrue {
		t.Errorf("expected RBACProfileNotProvisioned=True, got %+v", rbacCond)
	}
	if rbacCond.Reason != infrav1alpha1.ReasonRBACProfileNotReady {
		t.Errorf("reason=%q, want %q", rbacCond.Reason, infrav1alpha1.ReasonRBACProfileNotReady)
	}

	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) > 0 {
		t.Errorf("no Job expected when Gate 4 blocked, got %d", len(jobList.Items))
	}
}
