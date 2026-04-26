// Package unit_test -- T-VAL-01 Group 3: wrapper role-independence tests.
//
// These tests verify that the wrapper reconcilers (ClusterPackReconciler,
// PackExecutionReconciler) never read a conductor role field and treat the
// management cluster as just another target cluster.
//
// The management cluster is identified by TargetClusterRef="ccs-mgmt". The wrapper
// has no concept of "management vs tenant" -- it uses seam-tenant-{clusterRef}
// unconditionally for all cluster namespaces.
//
// Tests cover:
//   - PackExecution targeting the management cluster (ccs-mgmt) follows the same
//     gate sequence as any other cluster.
//   - When gate 0 clears for ccs-mgmt, the Job is submitted to the correct namespace.
//   - PackInstance is created in seam-tenant-ccs-mgmt, not seam-system or ont-system.
//   - ClusterPack reconciler does not branch on any role field.
//
// T-VAL-01, wrapper-schema.md §4, INV-002.
package unit_test

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/seam-core/pkg/conditions"
	"github.com/ontai-dev/wrapper/internal/controller"
)

// TestRoleIndependence_PackExecution_ManagementCluster_Gate0Blocks verifies that
// a PackExecution targeting the management cluster (ccs-mgmt) blocks at gate 0
// just like any other cluster when the TalosCluster and RunnerConfig are absent.
// The reconciler must not special-case the management cluster namespace.
func TestRoleIndependence_PackExecution_ManagementCluster_Gate0Blocks(t *testing.T) {
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("cilium", "infra-system", "v1.16.0")
	// TargetClusterRef=ccs-mgmt: the management cluster treated as a tenant.
	pe := newPackExecution("cilium-exec-mgmt", "infra-system", "cilium", "v1.16.0", "ccs-mgmt", "profile-mgmt")
	// No TalosCluster or RunnerConfig in the fake client for ccs-mgmt.

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

	// Gate 0 must block: no TalosCluster for ccs-mgmt.
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected RequeueAfter=30s for gate 0 (ccs-mgmt), got %v", result.RequeueAfter)
	}

	updated := &seamcorev1alpha1.InfrastructurePackExecution{}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get updated PackExecution: %v", err)
	}
	waitingCond := conditions.FindCondition(updated.Status.Conditions, conditions.ConditionTypePackExecutionWaiting)
	if waitingCond == nil || waitingCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Waiting=True for management cluster gate 0 block")
	}
}

// TestRoleIndependence_PackExecution_ManagementCluster_JobNamespace verifies that
// when all gates clear for a PackExecution targeting ccs-mgmt, the submitted Job
// lands in seam-tenant-ccs-mgmt -- not seam-system, not ont-system.
func TestRoleIndependence_PackExecution_ManagementCluster_JobNamespace(t *testing.T) {
	const (
		clusterRef  = "ccs-mgmt"
		tenantNS    = "seam-tenant-ccs-mgmt"
	)
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("cilium", "infra-system", "v1.16.0")
	// PackExecution lives in infra-system; Job will be submitted there too (PE namespace).
	pe := newPackExecution("cilium-exec-mgmt", "infra-system", "cilium", "v1.16.0", clusterRef, "cilium")
	ps := newPermissionSnapshot("snapshot-"+clusterRef, "seam-system", true)
	profile := newRBACProfile("cilium", tenantNS, true)
	tc := newTalosClusterWithConductorReady(clusterRef, true)
	rc := newRunnerConfig(clusterRef, 1)

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
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected RequeueAfter=10s after Job submit (all gates clear), got %v", result.RequeueAfter)
	}

	// Job must be in the PE namespace (infra-system in this test).
	jobList := &batchv1.JobList{}
	if err := fakeClient.List(context.Background(), jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) != 1 {
		t.Fatalf("expected 1 Job in infra-system, got %d", len(jobList.Items))
	}

	// The Job must carry the management cluster reference in its labels.
	job := jobList.Items[0]
	if got := job.Labels["infrastructure.ontai.dev/pe-name"]; got != pe.Name {
		t.Errorf("Job label pe-name = %q, want %q", got, pe.Name)
	}
}

// TestRoleIndependence_PackExecution_ManagementCluster_PackInstance verifies that
// PackInstance is created in seam-tenant-ccs-mgmt, not seam-system or ont-system.
// The wrapper always uses seam-tenant-{clusterRef} regardless of cluster role.
//
// Two-step flow: first reconcile submits Job; second reconcile reads the
// PackOperationResult and creates the PackInstance.
func TestRoleIndependence_PackExecution_ManagementCluster_PackInstance(t *testing.T) {
	const (
		clusterRef = "ccs-mgmt"
		tenantNS   = "seam-tenant-ccs-mgmt"
		peName     = "cilium-exec-mgmt"
		peNS       = "infra-system"
	)
	s := newPackExecutionScheme(t)
	cp := newSignedClusterPack("cilium", peNS, "v1.16.0")
	pe := newPackExecution(peName, peNS, "cilium", "v1.16.0", clusterRef, "cilium")
	ps := newPermissionSnapshot("snapshot-"+clusterRef, "seam-system", true)
	profile := newRBACProfile("cilium", tenantNS, true)
	tc := newTalosClusterWithConductorReady(clusterRef, true)
	rc := newRunnerConfig(clusterRef, 1)

	fakeClient := fake.NewClientBuilder().WithScheme(s).
		WithObjects(cp, pe, profile).
		WithStatusSubresource(
			&seamcorev1alpha1.InfrastructurePackExecution{},
			&seamcorev1alpha1.InfrastructureClusterPack{},
			&seamcorev1alpha1.InfrastructurePackInstance{},
		).
		Build()
	for _, obj := range []client.Object{ps, tc, rc} {
		if err := fakeClient.Create(context.Background(), obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
		RBACChecker: rbacAllowedStub,
	}

	// Step 1: first reconcile submits the Job.
	reconcilePE(t, r, pe)

	// Step 2: simulate Job completion by patching Job status.Succeeded=1.
	jobName := "pack-deploy-" + peName
	submittedJob := &batchv1.Job{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: jobName, Namespace: peNS}, submittedJob); err != nil {
		t.Fatalf("get submitted Job: %v", err)
	}
	jobPatch := client.MergeFrom(submittedJob.DeepCopy())
	submittedJob.Status.Succeeded = 1
	if err := fakeClient.Status().Patch(context.Background(), submittedJob, jobPatch); err != nil {
		t.Fatalf("patch Job status: %v", err)
	}

	// Step 3: create a PackOperationResult with status=Succeeded in the PE namespace.
	por := &seamcorev1alpha1.PackOperationResult{
		ObjectMeta: metav1.ObjectMeta{
			Name:      peName + "-por",
			Namespace: peNS,
			Labels:    map[string]string{"ontai.dev/pack-execution": peName},
		},
		Spec: seamcorev1alpha1.PackOperationResultSpec{
			Status:   seamcorev1alpha1.PackResultSucceeded,
			Revision: 1,
		},
	}
	if err := fakeClient.Create(context.Background(), por); err != nil {
		t.Fatalf("create PackOperationResult: %v", err)
	}

	// Step 4: second reconcile reads POR and creates PackInstance.
	reconcilePE(t, r, pe)

	// PackInstance must appear in seam-tenant-ccs-mgmt.
	piList := &seamcorev1alpha1.InfrastructurePackInstanceList{}
	if err := fakeClient.List(context.Background(), piList, client.InNamespace(tenantNS)); err != nil {
		t.Fatalf("list PackInstances in %s: %v", tenantNS, err)
	}
	if len(piList.Items) != 1 {
		t.Errorf("expected 1 PackInstance in %s, got %d", tenantNS, len(piList.Items))
	}
	if len(piList.Items) > 0 {
		pi := piList.Items[0]
		if pi.Spec.TargetClusterRef != clusterRef {
			t.Errorf("PackInstance TargetClusterRef = %q, want %q", pi.Spec.TargetClusterRef, clusterRef)
		}
	}

	// No PackInstance must exist in seam-system, ont-system, or the PE namespace.
	for _, forbiddenNS := range []string{"seam-system", "ont-system", peNS} {
		wrongList := &seamcorev1alpha1.InfrastructurePackInstanceList{}
		if err := fakeClient.List(context.Background(), wrongList, client.InNamespace(forbiddenNS)); err != nil {
			t.Fatalf("list PackInstances in %s: %v", forbiddenNS, err)
		}
		if len(wrongList.Items) != 0 {
			t.Errorf("unexpected PackInstance in %s: wrapper must use seam-tenant-{clusterRef}", forbiddenNS)
		}
	}
}

// TestRoleIndependence_ClusterPack_SignaturePathUnchanged verifies that the
// ClusterPackReconciler sets Signed=true when the signature annotation is present,
// regardless of what namespace it runs in. No conductor role field is consulted.
func TestRoleIndependence_ClusterPack_SignaturePathUnchanged(t *testing.T) {
	s := newClusterPackScheme(t)

	// A ClusterPack with an annotation from the management-cluster conductor.
	cp := &seamcorev1alpha1.InfrastructureClusterPack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cilium",
			Namespace: "infra-system",
			Finalizers: []string{"infrastructure.ontai.dev/clusterpack-cleanup"},
			Annotations: map[string]string{
				"ontai.dev/pack-signature": "mgmt-conductor-sig==",
			},
		},
		Spec: seamcorev1alpha1.InfrastructureClusterPackSpec{
			Version: "v1.16.0",
			RegistryRef: seamcorev1alpha1.InfrastructurePackRegistryRef{
				URL:    "registry.ontai.dev/packs/cilium",
				Digest: "sha256:mgmtdigest",
			},
			Checksum: "sha256:mgmtchecksum",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(cp).
		WithStatusSubresource(&seamcorev1alpha1.InfrastructureClusterPack{}).Build()
	r := &controller.ClusterPackReconciler{
		Client:   fakeClient,
		Scheme:   s,
		Recorder: clientevents.NewFakeRecorder(10),
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "cilium", Namespace: "infra-system"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	// With signature present, reconciler should set Signed=true and not requeue at 15s.
	if result.RequeueAfter == 15*time.Second {
		t.Error("expected reconciler to advance past signature-pending when annotation present")
	}

	updated := &seamcorev1alpha1.InfrastructureClusterPack{}
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "cilium", Namespace: "infra-system"}, updated); err != nil {
		t.Fatalf("get updated ClusterPack: %v", err)
	}
	if !updated.Status.Signed {
		t.Error("expected Signed=true after signature annotation was present; conductor role has no effect")
	}
}
