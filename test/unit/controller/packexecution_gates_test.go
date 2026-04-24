// Package controller_test -- AC-2 ClusterPack deploy gate chain acceptance contract tests.
//
// AC-2: A PackExecution proceeds to Job submission only when all five gates
// pass in order:
//   - Gate 0: ConductorReady (RunnerConfig has at least one capability)
//   - Gate 1: ClusterPack Signed=true
//   - Gate 2: ClusterPack not Revoked
//   - Gate 3: PermissionSnapshot is current (Fresh=True)
//   - Gate 4: RBACProfile is provisioned
//
// When any gate blocks, the specific blocking condition must be set on the
// PackExecution status and no Kueue Job may be submitted.
// When all gates pass, a Kueue pack-deploy Job is submitted.
//
// These tests constitute the acceptance contract gate for AC-2.
// They supplement the gate coverage in packinstance_lifecycle_test.go.
//
// wrapper-schema.md §4 PackExecution gates.
package controller_test

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/seam-core/pkg/conditions"
	"github.com/ontai-dev/wrapper/internal/controller"
)

// TestAC2_Gate1_PackExecution_BlockedWhenUnsigned verifies that a PackExecution
// is blocked with PackSignaturePending=True when ClusterPack.Status.Signed=false.
// No Kueue Job must be submitted. Gate 1 contract.
// AC-2 gate: ClusterPack signature contract.
func TestAC2_Gate1_PackExecution_BlockedWhenUnsigned(t *testing.T) {
	s := buildTestScheme(t)
	cp := newSignedCP("unsigned-pack-ac2", "v1.0.0", "infra-system")
	cp.Status.Signed = false
	cp.Annotations["ontai.dev/pack-signature"] = ""
	pe := newPE("pe-ac2-gate1", "unsigned-pack-ac2", "v1.0.0", cp.UID, "cluster-ac2g1", "profile-ac2g1", "infra-system")
	tc := newTalosCluster("cluster-ac2g1", true)
	rc := newRunnerConfig("cluster-ac2g1", 1)

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}).
		Build()
	ctx := context.Background()
	for _, obj := range []client.Object{tc, rc} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("AC-2 gate1: create %T: %v", obj, err)
		}
	}
	r := &controller.PackExecutionReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(16),
	}

	result := reconcilePackExecution(t, r, "pe-ac2-gate1", "infra-system")

	if result.RequeueAfter == 0 {
		t.Error("AC-2 gate1: expected requeue when signature pending, got no requeue")
	}
	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("AC-2 gate1: get PackExecution: %v", err)
	}
	cond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackSignaturePending)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("AC-2 gate1: PackSignaturePending not True when Signed=false; cond=%v", cond)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("AC-2 gate1: list Jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("AC-2 gate1: Job submitted despite unsigned pack, got %d Jobs", len(jobs.Items))
	}
}

// TestAC2_Gate3_PackExecution_BlockedWhenSnapshotStale verifies that a PackExecution
// is blocked with PermissionSnapshotOutOfSync=True when the PermissionSnapshot has
// Fresh=False. No Kueue Job must be submitted. Gate 3 contract.
// AC-2 gate: PermissionSnapshot freshness contract.
func TestAC2_Gate3_PackExecution_BlockedWhenSnapshotStale(t *testing.T) {
	s := buildTestScheme(t)
	cp := newSignedCP("stale-snap-pack-ac2", "v1.0.0", "infra-system")
	pe := newPE("pe-ac2-gate3", "stale-snap-pack-ac2", "v1.0.0", cp.UID, "cluster-ac2g3", "profile-ac2g3", "infra-system")
	tc := newTalosCluster("cluster-ac2g3", true)
	rc := newRunnerConfig("cluster-ac2g3", 1)
	ps := newPermissionSnapshot("snapshot-cluster-ac2g3", "seam-system", false) // Fresh=False

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}).
		Build()
	ctx := context.Background()
	for _, obj := range []client.Object{tc, rc, ps} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("AC-2 gate3: create %T: %v", obj, err)
		}
	}
	r := &controller.PackExecutionReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(16),
	}

	result := reconcilePackExecution(t, r, "pe-ac2-gate3", "infra-system")

	if result.RequeueAfter == 0 {
		t.Error("AC-2 gate3: expected requeue when snapshot stale, got no requeue")
	}
	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("AC-2 gate3: get PackExecution: %v", err)
	}
	cond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePermissionSnapshotOutOfSync)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("AC-2 gate3: PermissionSnapshotOutOfSync not True when snapshot stale; cond=%v", cond)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("AC-2 gate3: list Jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("AC-2 gate3: Job submitted despite stale snapshot, got %d Jobs", len(jobs.Items))
	}
}

// TestAC2_Gate4_PackExecution_BlockedWhenRBACNotProvisioned verifies that a
// PackExecution is blocked with RBACProfileNotProvisioned=True when the
// RBACProfile has provisioned=false. No Kueue Job must be submitted. Gate 4 contract.
// AC-2 gate: RBACProfile provisioning contract.
func TestAC2_Gate4_PackExecution_BlockedWhenRBACNotProvisioned(t *testing.T) {
	s := buildTestScheme(t)
	cp := newSignedCP("rbac-pack-ac2", "v1.0.0", "infra-system")
	pe := newPE("pe-ac2-gate4", "rbac-pack-ac2", "v1.0.0", cp.UID, "cluster-ac2g4", "profile-ac2g4", "infra-system")
	tc := newTalosCluster("cluster-ac2g4", true)
	rc := newRunnerConfig("cluster-ac2g4", 1)
	ps := newPermissionSnapshot("snapshot-cluster-ac2g4", "seam-system", true)
	rp := newRBACProfile("profile-ac2g4", "seam-tenant-cluster-ac2g4", false) // provisioned=false

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}).
		Build()
	ctx := context.Background()
	for _, obj := range []client.Object{tc, rc, ps, rp} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("AC-2 gate4: create %T: %v", obj, err)
		}
	}
	r := &controller.PackExecutionReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(16),
	}

	result := reconcilePackExecution(t, r, "pe-ac2-gate4", "infra-system")

	if result.RequeueAfter == 0 {
		t.Error("AC-2 gate4: expected requeue when RBACProfile not provisioned, got no requeue")
	}
	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("AC-2 gate4: get PackExecution: %v", err)
	}
	cond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeRBACProfileNotProvisioned)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("AC-2 gate4: RBACProfileNotProvisioned not True when unprovisioned; cond=%v", cond)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("AC-2 gate4: list Jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("AC-2 gate4: Job submitted despite unprovisioned RBACProfile, got %d Jobs", len(jobs.Items))
	}
}

// TestAC2_Gate2_PackExecution_BlockedWhenRevoked verifies that a PackExecution
// is blocked with PackRevoked=True when the referenced ClusterPack has
// Revoked=True. No Kueue Job must be submitted and no requeue occurs.
// AC-2 gate: ClusterPack revocation contract (human intervention required).
func TestAC2_Gate2_PackExecution_BlockedWhenRevoked(t *testing.T) {
	s := buildTestScheme(t)
	cp := newSignedCP("revoked-pack-ac2", "v1.0.0", "infra-system")
	conditions.SetCondition(
		&cp.Status.Conditions,
		conditions.ConditionTypeClusterPackRevoked,
		metav1.ConditionTrue,
		"ManualRevocation",
		"Pack revoked by operator.",
		1,
	)
	pe := newPE("pe-ac2-gate2", "revoked-pack-ac2", "v1.0.0", cp.UID, "cluster-ac2g2", "profile-ac2g2", "infra-system")
	tc := newTalosCluster("cluster-ac2g2", true)
	rc := newRunnerConfig("cluster-ac2g2", 1)

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}).
		Build()
	ctx := context.Background()
	for _, obj := range []client.Object{tc, rc} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("AC-2 gate2: create %T: %v", obj, err)
		}
	}
	r := &controller.PackExecutionReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(16),
	}

	result := reconcilePackExecution(t, r, "pe-ac2-gate2", "infra-system")

	// Revocation requires human intervention — no requeue.
	if result.RequeueAfter != 0 {
		t.Errorf("AC-2 gate2: revoked pack must not requeue, got RequeueAfter=%v", result.RequeueAfter)
	}
	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("AC-2 gate2: get PackExecution: %v", err)
	}
	cond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackRevoked)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("AC-2 gate2: PackRevoked not True when ClusterPack is revoked; cond=%v", cond)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("AC-2 gate2: list Jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("AC-2 gate2: Job submitted despite revoked pack, got %d Jobs", len(jobs.Items))
	}
}

// TestAC2_AllGatesPass_JobSubmitted verifies that when all five gates are satisfied,
// the PackExecutionReconciler submits a Kueue pack-deploy Job and the PackExecution
// status does not carry any blocking condition.
// AC-2 gate: all-gates-clear → Job submission contract.
func TestAC2_AllGatesPass_JobSubmitted(t *testing.T) {
	c, pe := allGatesSetup(t, "pe-ac2-all", "all-gates-pack", "v2.0.0", "cluster-all", "profile-all")
	r := &controller.PackExecutionReconciler{
		Client:   c,
		Scheme:   buildTestScheme(t),
		Recorder: clientevents.NewFakeRecorder(16),
	}

	result := reconcilePackExecution(t, r, "pe-ac2-all", "infra-system")

	// A requeue may occur (waiting for Job to complete) but no blocking requeue
	// must be set for an unresolvable gate condition.
	if result.RequeueAfter > 30*time.Second {
		t.Errorf("AC-2 all-gates: unexpected long requeue after gate pass: %v", result.RequeueAfter)
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("AC-2 all-gates: list Jobs: %v", err)
	}
	if len(jobs.Items) == 0 {
		t.Error("AC-2 all-gates: no Job submitted when all gates pass")
	}

	// No blocking conditions must remain set.
	ctx := context.Background()
	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("AC-2 all-gates: get PackExecution: %v", err)
	}
	for _, ct := range []string{
		conditions.ConditionTypePackSignaturePending,
		conditions.ConditionTypePermissionSnapshotOutOfSync,
		conditions.ConditionTypeRBACProfileNotProvisioned,
		conditions.ConditionTypePackRevoked,
	} {
		if cond := conditions.FindCondition(updated.Status.Conditions, ct); cond != nil && cond.Status == metav1.ConditionTrue {
			t.Errorf("AC-2 all-gates: blocking condition %q is True after all gates pass", ct)
		}
	}
}
