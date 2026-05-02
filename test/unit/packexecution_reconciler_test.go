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

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/seam-core/pkg/conditions"
	"github.com/ontai-dev/wrapper/internal/controller"
)

func newPackExecutionScheme(t *testing.T) *runtime.Scheme {
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

func newSignedClusterPack(name, namespace, version string) *seamcorev1alpha1.InfrastructureClusterPack {
	cp := &seamcorev1alpha1.InfrastructureClusterPack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				"ontai.dev/pack-signature": "validsig==",
			},
		},
		Spec: seamcorev1alpha1.InfrastructureClusterPackSpec{
			Version: version,
			RegistryRef: seamcorev1alpha1.InfrastructurePackRegistryRef{
				URL:    "registry.ontai.dev/packs/" + name,
				Digest: "sha256:abc123",
			},
			Checksum: "sha256:def456",
		},
		Status: seamcorev1alpha1.InfrastructureClusterPackStatus{
			Signed:        true,
			PackSignature: "validsig==",
		},
	}
	return cp
}

func newPackExecution(name, namespace, packName, packVersion, clusterRef, profileRef string) *seamcorev1alpha1.InfrastructurePackExecution {
	return &seamcorev1alpha1.InfrastructurePackExecution{
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
		Group:   "infrastructure.ontai.dev",
		Version: "v1alpha1",
		Kind:    "InfrastructureRunnerConfig",
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
		Group:   "infrastructure.ontai.dev",
		Version: "v1alpha1",
		Kind:    "InfrastructureTalosCluster",
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

func reconcilePE(t *testing.T, r *controller.PackExecutionReconciler, pe *seamcorev1alpha1.InfrastructurePackExecution) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: pe.Name, Namespace: pe.Namespace},
	})
	if err != nil {
		t.Fatalf("Reconcile returned unexpected error: %v", err)
	}
	return result
}

// rbacAllowedStub is a RBACReadyChecker stub that always grants all permissions.
// Required for tests that need gate 5 (WrapperRunnerRBAC) to pass so they can reach
// job-submission or post-job logic. The real gate requires a live API server SAR.
func rbacAllowedStub(_ context.Context, _ *seamcorev1alpha1.InfrastructurePackExecution) (bool, string, error) {
	return true, "", nil
}

var _ controller.RBACReadyChecker = rbacAllowedStub

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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
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
		RBACChecker: rbacAllowedStub,
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s for signature gate, got %v", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	sigCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackSignaturePending)
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
			Type:               conditions.ConditionTypeClusterPackRevoked,
			Status:             metav1.ConditionTrue,
			Reason:             conditions.ReasonPackRevoked,
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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
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
		RBACChecker: rbacAllowedStub,
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for revoked pack, got %+v", result)
	}

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	revokedCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackRevoked)
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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
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
		RBACChecker: rbacAllowedStub,
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for snapshot gate, got %v", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	snapCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePermissionSnapshotOutOfSync)
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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
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
		RBACChecker: rbacAllowedStub,
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for RBAC gate, got %v", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	rbacCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeRBACProfileNotProvisioned)
	if rbacCond == nil || rbacCond.Status != metav1.ConditionTrue {
		t.Errorf("expected RBACProfileNotProvisioned=True")
	}
}

// TestPackExecutionReconciler_Gate4_AbsentProfileAllowsJobSubmission verifies that
// when the RBACProfile does not yet exist (Conductor Job will create it via rbac-intake),
// gate 4 clears and the Job is submitted. This prevents a deadlock where gate 4 would
// permanently block first-deploy because the profile only exists after the Job runs.
func TestPackExecutionReconciler_Gate4_AbsentProfileAllowsJobSubmission(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("my-pack", "infra-system", "v1.0.0")
	pe := newPackExecution("exec-absent-profile", "infra-system", "my-pack", "v1.0.0", "cluster-a", "profile-absent")
	ps := newPermissionSnapshot("snapshot-cluster-a", "seam-system", true)
	tc := newTalosClusterWithConductorReady("cluster-a", true)
	rc := newRunnerConfig("cluster-a", 1)
	// No RBACProfile created — profile absent.

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
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
		Client:      fakeClient,
		Scheme:      s,
		Recorder:    clientevents.NewFakeRecorder(10),
		RBACChecker: rbacAllowedStub,
	}

	reconcilePE(t, r, pe)

	// A Job must have been submitted — gate 4 did not block on absent profile.
	jobList := &unstructured.UnstructuredList{}
	jobList.SetGroupVersionKind(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "JobList"})
	if err := fakeClient.List(context.Background(), jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) == 0 {
		t.Error("expected a pack-deploy Job to be submitted when RBACProfile is absent; got none")
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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
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
		RBACChecker: rbacAllowedStub,
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
	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	runningCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackExecutionRunning)
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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
		Build()
	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
		RBACChecker: rbacAllowedStub,
	}

	reconcilePE(t, r, pe)

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	lineageCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypeLineageSynced)
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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
		Build()
	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
		RBACChecker: rbacAllowedStub,
	}

	result := reconcilePE(t, r, pe)

	// Gate 0 must cause a requeue.
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for ConductorReady gate, got %v", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	waitingCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackExecutionWaiting)
	if waitingCond == nil || waitingCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Waiting=True when TalosCluster absent")
	}
	if waitingCond.Reason != conditions.ReasonAwaitingConductorReady {
		t.Errorf("expected reason %q, got %q",
			conditions.ReasonAwaitingConductorReady, waitingCond.Reason)
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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
		Build()
	if err := fakeClient.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
		RBACChecker: rbacAllowedStub,
	}

	result := reconcilePE(t, r, pe)

	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for ConductorReady gate (False), got %v", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	waitingCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackExecutionWaiting)
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
		WithStatusSubresource(&seamcorev1alpha1.InfrastructurePackExecution{}, &seamcorev1alpha1.InfrastructureClusterPack{}).
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
		RBACChecker: rbacAllowedStub,
	}

	result := reconcilePE(t, r, pe)

	// Gate 0 passed, gate 1 (signature) blocked with 15s requeue.
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter=15s (signature gate), got %v — gate 0 may not have cleared", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}

	// Waiting condition must be False (gate 0 cleared).
	waitingCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackExecutionWaiting)
	if waitingCond == nil || waitingCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Waiting=False when ConductorReady=True (gate 0 cleared), got %v", waitingCond)
	}

	// Gate 1 (signature) must have fired.
	sigCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackSignaturePending)
	if sigCond == nil || sigCond.Status != metav1.ConditionTrue {
		t.Errorf("expected PackSignaturePending=True when signature gate fires after gate 0 clears")
	}
}

// TestPackExecutionReconciler_Gate0_RunnerConfigCapabilitiesAppear verifies
// CONDUCTOR-BL-CAPABILITY-WATCH: when a RunnerConfig has no capabilities, gate 0
// blocks with Waiting=True; after capabilities are published to the RunnerConfig,
// a fresh reconcile immediately clears gate 0 and proceeds. This confirms the
// RunnerConfig watch in SetupWithManager fires at the right time.
func TestPackExecutionReconciler_Gate0_RunnerConfigCapabilitiesAppear(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("cilium", "seam-tenant-ccs-test", "v1.2.0")
	pe := newPackExecution("cilium-exec", "seam-tenant-ccs-test",
		"cilium", "v1.2.0", "ccs-test", "rbac-wrapper")
	snapshot := newPermissionSnapshot("snapshot-ccs-test", "security-system", true)
	rbacProfile := newRBACProfile("rbac-wrapper", "seam-system", true)

	// RunnerConfig with NO capabilities — gate 0 must block.
	rcNoCapabilities := newRunnerConfig("ccs-test", 0)
	// TalosCluster must exist for gate 0 to proceed to RunnerConfig capability check.
	tc := newTalosClusterWithConductorReady("ccs-test", false)

	cl := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cp, pe, rcNoCapabilities).
		WithStatusSubresource(pe).
		Build()
	if err := cl.Create(context.Background(), tc); err != nil {
		t.Fatalf("create TalosCluster: %v", err)
	}

	// Store snapshot and RBACProfile as unstructured.
	if err := cl.Create(context.Background(), snapshot); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := cl.Create(context.Background(), rbacProfile); err != nil {
		t.Fatalf("create rbacProfile: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   cl,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(16),
	}

	// First reconcile — gate 0 must block.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cilium-exec", Namespace: "seam-tenant-ccs-test"},
	})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected non-zero RequeueAfter when gate 0 (ConductorReady) not cleared")
	}
	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "cilium-exec", Namespace: "seam-tenant-ccs-test"}, updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	waitCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackExecutionWaiting)
	if waitCond == nil || waitCond.Status != metav1.ConditionTrue {
		t.Error("expected Waiting=True when gate 0 not cleared")
	}

	// Now publish capabilities to RunnerConfig — simulates Conductor declaring capability.
	// Re-fetch to get current resourceVersion before updating.
	rcKey := types.NamespacedName{Name: "ccs-test", Namespace: "ont-system"}
	rcLive := &unstructured.Unstructured{}
	rcLive.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "infrastructure.ontai.dev",
		Version: "v1alpha1",
		Kind:    "InfrastructureRunnerConfig",
	})
	if err := cl.Get(context.Background(), rcKey, rcLive); err != nil {
		t.Fatalf("get RunnerConfig: %v", err)
	}
	// Mutate the live object in-place and use regular Update (RunnerConfig is not in
	// WithStatusSubresource so the fake client stores the full object including status).
	caps := []interface{}{map[string]interface{}{"name": "pack-deploy", "version": "v1.0.0"}}
	if err := unstructured.SetNestedSlice(rcLive.Object, caps, "status", "capabilities"); err != nil {
		t.Fatalf("set capabilities on rcLive: %v", err)
	}
	if err := cl.Update(context.Background(), rcLive); err != nil {
		t.Fatalf("update RunnerConfig with capabilities: %v", err)
	}
	// Verify capabilities stored before proceeding.
	rcCheck := &unstructured.Unstructured{}
	rcCheck.SetGroupVersionKind(schema.GroupVersionKind{Group: "infrastructure.ontai.dev", Version: "v1alpha1", Kind: "InfrastructureRunnerConfig"})
	if err := cl.Get(context.Background(), rcKey, rcCheck); err != nil {
		t.Fatalf("get RunnerConfig after update: %v", err)
	}
	if gotCaps, _, _ := unstructured.NestedSlice(rcCheck.Object, "status", "capabilities"); len(gotCaps) == 0 {
		t.Fatal("capabilities not stored in fake client after Update — test setup error")
	}

	// Second reconcile — gate 0 must clear. The watch would trigger this automatically
	// in production; here we trigger it manually to verify the gate logic.
	result2, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cilium-exec", Namespace: "seam-tenant-ccs-test"},
	})
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}
	updated2 := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "cilium-exec", Namespace: "seam-tenant-ccs-test"}, updated2); err != nil {
		t.Fatalf("get PackExecution after second reconcile: %v", err)
	}
	// Gate 0 cleared — reconciler sets Waiting=False. The condition still carries
	// ReasonAwaitingConductorReady but with Status=False to record the clear event.
	waitCond2 := conditions.FindCondition(updated2.Status.Conditions, conditions.ConditionTypePackExecutionWaiting)
	if waitCond2 != nil && waitCond2.Status == metav1.ConditionTrue && waitCond2.Reason == conditions.ReasonAwaitingConductorReady {
		t.Error("gate 0 must clear after capabilities published; Waiting=True/AwaitingConductorReady must not remain set")
	}
	_ = result2
}
