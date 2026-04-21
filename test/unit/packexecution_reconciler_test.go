package unit_test

import (
	"context"
	"testing"
	"time"

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

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

func newPackExecutionScheme(t *testing.T) *runtime.Scheme {
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

func newSignedClusterPack(name, namespace, version string) *infrav1alpha1.ClusterPack {
	cp := &infrav1alpha1.ClusterPack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				"ontai.dev/pack-signature": "validsig==",
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
			PackSignature: "validsig==",
		},
	}
	return cp
}

func newPackExecution(name, namespace, packName, packVersion, clusterRef, profileRef string) *infrav1alpha1.PackExecution {
	return &infrav1alpha1.PackExecution{
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
			AdmissionProfileRef: profileRef,
		},
	}
}

func newPermissionSnapshot(name, namespace string, current bool) *unstructured.Unstructured {
	ps := &unstructured.Unstructured{}
	ps.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "PermissionSnapshot",
	})
	ps.SetName(name)
	ps.SetNamespace(namespace)
	condStatus := "False"
	if current {
		condStatus = "True"
	}
	_ = unstructured.SetNestedSlice(ps.Object, []interface{}{
		map[string]interface{}{
			"type":   "Fresh",
			"status": condStatus,
			"reason": "Synced",
		},
	}, "status", "conditions")
	return ps
}

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

// newRunnerConfig creates a fake RunnerConfig unstructured object in ont-system
// with capCount capability entries. capCount=0 leaves capabilities empty (gate 0
// treats this as conductor not yet ready). Fix 2. conductor-schema.md §5.
func newRunnerConfig(clusterName string, capCount int) *unstructured.Unstructured {
	rc := &unstructured.Unstructured{}
	rc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "runner.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RunnerConfig",
	})
	rc.SetName(clusterName)
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

// newTalosClusterWithConductorReady creates a fake TalosCluster unstructured object
// in seam-tenant-{clusterName} with the given ConductorReady condition status.
// Used to satisfy gate 0 in PackExecutionReconciler tests. Gap 27.
func newTalosClusterWithConductorReady(clusterName string, conductorReady bool) *unstructured.Unstructured {
	tc := &unstructured.Unstructured{}
	tc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "platform.ontai.dev",
		Version: "v1alpha1",
		Kind:    "TalosCluster",
	})
	tc.SetName(clusterName)
	tc.SetNamespace("seam-tenant-" + clusterName)

	condStatus := "False"
	if conductorReady {
		condStatus = "True"
	}
	_ = unstructured.SetNestedSlice(tc.Object, []interface{}{
		map[string]interface{}{
			"type":   "ConductorReady",
			"status": condStatus,
			"reason": "ConductorDeploymentAvailable",
		},
	}, "status", "conditions")
	return tc
}

func reconcilePE(t *testing.T, r *controller.PackExecutionReconciler, pe *infrav1alpha1.PackExecution) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: pe.Name, Namespace: pe.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	return result
}

// TestPackExecutionReconciler_Gate1_SignaturePending verifies gate 1 requeues when
// the ClusterPack is not yet signed (gate 0 already cleared via ConductorReady=True).
func TestPackExecutionReconciler_Gate1_SignaturePending(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-1", "infra-system", "my-pack", "v1.0.0", "cluster-a", "profile-a")
	// TalosCluster + RunnerConfig with capabilities satisfies gate 0; gate 1 is the first to block.
	tc := newTalosClusterWithConductorReady("cluster-a", true)
	rc := newRunnerConfig("cluster-a", 1)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}
	if err := fakeClient.Create(context.Background(), rc); err != nil {
		t.Fatalf("create RunnerConfig: %v", err)
	}
	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s for signature gate, got %v", result.RequeueAfter)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	sigCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackSignaturePending)
	if sigCond == nil || sigCond.Status != metav1.ConditionTrue {
		t.Errorf("expected PackSignaturePending=True")
	}
}

// TestPackExecutionReconciler_Gate2_PackRevoked verifies that a revoked ClusterPack
// sets PackRevoked on the PackExecution and does not requeue.
func TestPackExecutionReconciler_Gate2_PackRevoked(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("my-pack", "infra-system", "v1.0.0")
	cp.Status.Conditions = []metav1.Condition{
		{
			Type:               infrav1alpha1.ConditionTypeClusterPackRevoked,
			Status:             metav1.ConditionTrue,
			Reason:             infrav1alpha1.ReasonPackRevoked,
			Message:            "revoked by admin",
			LastTransitionTime: metav1.Now(),
		},
	}
	pe := newPackExecution("exec-revoked", "infra-system", "my-pack", "v1.0.0", "cluster-a", "profile-a")
	// TalosCluster + RunnerConfig with capabilities satisfies gate 0; gate 2 is the first to block.
	tc := newTalosClusterWithConductorReady("cluster-a", true)
	rc := newRunnerConfig("cluster-a", 1)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}
	if err := fakeClient.Create(context.Background(), rc); err != nil {
		t.Fatalf("create RunnerConfig: %v", err)
	}
	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for revoked pack, got %+v", result)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	revokedCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackRevoked)
	if revokedCond == nil || revokedCond.Status != metav1.ConditionTrue {
		t.Errorf("expected PackRevoked=True condition")
	}
}

// TestPackExecutionReconciler_Gate3_SnapshotOutOfSync verifies gate 3 requeues
// when the PermissionSnapshot is not current.
func TestPackExecutionReconciler_Gate3_SnapshotOutOfSync(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-snap", "infra-system", "my-pack", "v1.0.0", "cluster-a", "profile-a")
	ps := newPermissionSnapshot("snapshot-cluster-a", "seam-system", false)
	profile := newRBACProfile("profile-a", "seam-tenant-cluster-a", true)
	// TalosCluster + RunnerConfig with capabilities satisfies gate 0; gate 3 is the first to block.
	tc := newTalosClusterWithConductorReady("cluster-a", true)
	rc := newRunnerConfig("cluster-a", 1)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe, profile).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	// Add unstructured PermissionSnapshot.
	if err := fakeClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PermissionSnapshot: %v", err)
	}
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}
	if err := fakeClient.Create(context.Background(), rc); err != nil {
		t.Fatalf("create RunnerConfig: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for snapshot gate, got %v", result.RequeueAfter)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	snapCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePermissionSnapshotOutOfSync)
	if snapCond == nil || snapCond.Status != metav1.ConditionTrue {
		t.Errorf("expected PermissionSnapshotOutOfSync=True")
	}
}

// TestPackExecutionReconciler_Gate4_RBACProfileNotProvisioned verifies gate 4
// requeues when the RBACProfile is not yet provisioned.
func TestPackExecutionReconciler_Gate4_RBACProfileNotProvisioned(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-rbac", "infra-system", "my-pack", "v1.0.0", "cluster-a", "profile-a")
	ps := newPermissionSnapshot("snapshot-cluster-a", "seam-system", true)
	profile := newRBACProfile("profile-a", "seam-tenant-cluster-a", false)
	// TalosCluster + RunnerConfig with capabilities satisfies gate 0; gate 4 is the first to block.
	tc := newTalosClusterWithConductorReady("cluster-a", true)
	rc := newRunnerConfig("cluster-a", 1)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe, profile).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	if err := fakeClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PermissionSnapshot: %v", err)
	}
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}
	if err := fakeClient.Create(context.Background(), rc); err != nil {
		t.Fatalf("create RunnerConfig: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for RBAC gate, got %v", result.RequeueAfter)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	rbacCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeRBACProfileNotProvisioned)
	if rbacCond == nil || rbacCond.Status != metav1.ConditionTrue {
		t.Errorf("expected RBACProfileNotProvisioned=True")
	}
}

// TestPackExecutionReconciler_AllGatesClear_JobSubmitted verifies that when all
// five gates pass (gate 0: ConductorReady + gates 1-4), a pack-deploy Job is
// submitted with the Kueue queue label. Gap 27.
func TestPackExecutionReconciler_AllGatesClear_JobSubmitted(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-submit", "infra-system", "my-pack", "v1.0.0", "cluster-a", "profile-a")
	ps := newPermissionSnapshot("snapshot-cluster-a", "seam-system", true)
	profile := newRBACProfile("profile-a", "seam-tenant-cluster-a", true)
	// TalosCluster + RunnerConfig with capabilities satisfies gate 0.
	tc := newTalosClusterWithConductorReady("cluster-a", true)
	rc := newRunnerConfig("cluster-a", 1)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe, profile).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	if err := fakeClient.Create(context.Background(), ps); err != nil {
		t.Fatalf("create PermissionSnapshot: %v", err)
	}
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}
	if err := fakeClient.Create(context.Background(), rc); err != nil {
		t.Fatalf("create RunnerConfig: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePE(t, r, pe)

	// Expect requeue to poll Job status.
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected RequeueAfter=10s after Job submit, got %v", result.RequeueAfter)
	}

	// Verify Job was created.
	jobList := &unstructured.UnstructuredList{}
	jobList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "batch",
		Version: "v1",
		Kind:    "JobList",
	})
	if err := fakeClient.List(context.Background(), jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) != 1 {
		t.Fatalf("expected 1 Job, got %d", len(jobList.Items))
	}

	job := jobList.Items[0]
	labels := job.GetLabels()
	queueLabel, ok := labels["kueue.x-k8s.io/queue-name"]
	if !ok {
		t.Error("Job missing Kueue queue-name label — invariant violation")
	}
	if queueLabel != "pack-deploy-queue" {
		t.Errorf("expected queue-name=pack-deploy-queue, got %q", queueLabel)
	}

	// Verify PackExecution status shows Running.
	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	runningCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionRunning)
	if runningCond == nil || runningCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Running=True after Job submit")
	}
}

// TestPackExecutionReconciler_LineageSyncedInitialized verifies the LineageSynced
// condition is initialized on first reconcile.
func TestPackExecutionReconciler_LineageSyncedInitialized(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-lineage", "infra-system", "my-pack", "v1.0.0", "cluster-a", "profile-a")

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	reconcilePE(t, r, pe)

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	lineageCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypeLineageSynced)
	if lineageCond == nil {
		t.Fatal("expected LineageSynced condition")
	}
	if lineageCond.Status != metav1.ConditionFalse {
		t.Errorf("expected LineageSynced=False, got %v", lineageCond.Status)
	}
}

// --- ConductorReady gate (gate 0) tests --- Gap 27

// TestPackExecutionReconciler_Gate0_ConductorReadyAbsent verifies that when the
// target cluster's TalosCluster does not exist in seam-tenant-{clusterRef}, gate 0
// blocks Job submission, sets Waiting condition, and requeues.
func TestPackExecutionReconciler_Gate0_ConductorReadyAbsent(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-gate0-absent", "infra-system", "my-pack", "v1.0.0", "cluster-b", "profile-b")
	// No TalosCluster for cluster-b in the fake client.

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePE(t, r, pe)

	// Gate 0 must cause a requeue.
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for ConductorReady gate, got %v", result.RequeueAfter)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	waitingCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionWaiting)
	if waitingCond == nil || waitingCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Waiting=True when TalosCluster absent")
	}
	if waitingCond.Reason != infrav1alpha1.ReasonAwaitingConductorReady {
		t.Errorf("expected reason %q, got %q",
			infrav1alpha1.ReasonAwaitingConductorReady, waitingCond.Reason)
	}
}

// TestPackExecutionReconciler_Gate0_ConductorReadyFalse verifies that when the
// target cluster's TalosCluster has ConductorReady=False, gate 0 blocks Job
// submission, sets Waiting condition, and requeues.
func TestPackExecutionReconciler_Gate0_ConductorReadyFalse(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-gate0-false", "infra-system", "my-pack", "v1.0.0", "cluster-b", "profile-b")
	// TalosCluster exists but ConductorReady=False.
	tc := newTalosClusterWithConductorReady("cluster-b", false)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for ConductorReady gate (False), got %v", result.RequeueAfter)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	waitingCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionWaiting)
	if waitingCond == nil || waitingCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Waiting=True when ConductorReady=False")
	}
}

// TestPackExecutionReconciler_Gate0_ConductorReadyTrue_ProceedsToSignatureGate
// verifies that when ConductorReady=True on the target TalosCluster, gate 0 clears
// and the reconciler proceeds to gate 1 (signature). The Waiting condition is cleared.
func TestPackExecutionReconciler_Gate0_ConductorReadyTrue_ProceedsToSignatureGate(t *testing.T) {
	s := newPackExecutionScheme(t)
	// Use an unsigned ClusterPack so gate 1 (signature) blocks after gate 0 passes.
	cp := newClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-gate0-true", "infra-system", "my-pack", "v1.0.0", "cluster-c", "profile-c")
	// TalosCluster + RunnerConfig with capabilities — gate 0 passes.
	tc := newTalosClusterWithConductorReady("cluster-c", true)
	rc := newRunnerConfig("cluster-c", 1)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&infrav1alpha1.PackExecution{}, &infrav1alpha1.ClusterPack{}).
		Build()
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}
	if err := fakeClient.Create(context.Background(), rc); err != nil {
		t.Fatalf("create RunnerConfig: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result := reconcilePE(t, r, pe)

	// Gate 0 passed, gate 1 (signature) blocked with 15s requeue.
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s (signature gate), got %v — gate 0 may not have cleared", result.RequeueAfter)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}

	// Waiting condition must be False (gate 0 cleared).
	waitingCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionWaiting)
	if waitingCond == nil || waitingCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Waiting=False when ConductorReady=True (gate 0 cleared), got %v", waitingCond)
	}

	// Gate 1 (signature) must have fired.
	sigCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackSignaturePending)
	if sigCond == nil || sigCond.Status != metav1.ConditionTrue {
		t.Errorf("expected PackSignaturePending=True when signature gate fires after gate 0 clears")
	}
}
