// Package controller_test — Kueue Job scheduling unit tests.
//
// Workstream 2: Kueue Job scheduling via PackExecutionReconciler.
//
// Wrapper submits a single Kueue Job per PackExecution via Kueue admission.
// No DAG, no multi-Job orchestration. One pack-deploy Job per PackExecution.
// wrapper-design.md §4, §6.
//
// Tests cover:
//   - All five gates satisfied → Job submitted with correct Kueue queue label,
//     target cluster credentials reference, pack artifact reference env vars;
//     Job carries no DAG execution fields.
//   - Kueue Job fails → PackExecution status updated to Failed with JobFailed reason;
//     reconciler returns without requeue (human intervention required).
//   - Kueue Job succeeds → PackExecution status updated to Succeeded; OperationResultRef
//     set; PackInstance created.
//   - Second reconcile with running Job → no duplicate Job created; Running=True;
//     requeue after 10s.
package controller_test

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
	"github.com/ontai-dev/wrapper/internal/controller"
)

// jobSubmissionSetup returns a fake client with all five gates satisfied and no
// pre-existing Job. The returned PackExecution and ClusterPack can be inspected
// after reconcile to verify Job labels and env vars.
func jobSubmissionSetup(t *testing.T) (client.Client, *infrav1alpha1.PackExecution) {
	t.Helper()
	return allGatesSetup(t, "pe-job", "cilium-pack", "v1.2.3", "cluster-g", "profile-g")
}

// TestJobSubmission_KueueQueueLabel verifies that a submitted pack-deploy Job
// carries the required kueue.x-k8s.io/queue-name=pack-deploy-queue label.
// Without this label the Job will not be admitted by Kueue. wrapper-design.md §4.
func TestJobSubmission_KueueQueueLabel(t *testing.T) {
	fakeClient, pe := jobSubmissionSetup(t)

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: record.NewFakeRecorder(32),
	}

	reconcilePackExecution(t, r, pe.Name, pe.Namespace)

	// Fetch the Job the reconciler created.
	ctx := context.Background()
	job := &batchv1.Job{}
	jobName := packDeployJobName(pe.Name)
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: pe.Namespace}, job); err != nil {
		t.Fatalf("Job %q not created: %v", jobName, err)
	}

	// Kueue admission label — must be present and correct.
	const kueueLabel = "kueue.x-k8s.io/queue-name"
	if v, ok := job.Labels[kueueLabel]; !ok {
		t.Errorf("Job missing Kueue label %q", kueueLabel)
	} else if v != "pack-deploy-queue" {
		t.Errorf("Kueue label %q=%q, want pack-deploy-queue", kueueLabel, v)
	}

	// Part-of label.
	if v := job.Labels["app.kubernetes.io/part-of"]; v != "wrapper" {
		t.Errorf("app.kubernetes.io/part-of=%q, want wrapper", v)
	}

	// PE name label — used for filtering.
	if v := job.Labels["infra.ontai.dev/pe-name"]; v != pe.Name {
		t.Errorf("infra.ontai.dev/pe-name=%q, want %q", v, pe.Name)
	}
}

// TestJobSubmission_PackArtifactRefEnvVars verifies that the submitted Job container
// carries PACK_REGISTRY_REF, PACK_CHECKSUM, and PACK_SIGNATURE environment variables
// derived from the ClusterPack spec and status. wrapper-design.md §6.
func TestJobSubmission_PackArtifactRefEnvVars(t *testing.T) {
	const (
		cpName    = "cilium-pack"
		cpVersion = "v1.2.3"
	)

	fakeClient, pe := jobSubmissionSetup(t)

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: record.NewFakeRecorder(32),
	}

	reconcilePackExecution(t, r, pe.Name, pe.Namespace)

	ctx := context.Background()
	job := &batchv1.Job{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: packDeployJobName(pe.Name), Namespace: pe.Namespace}, job); err != nil {
		t.Fatalf("Job not found: %v", err)
	}

	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	envMap := make(map[string]string)
	for _, e := range containers[0].Env {
		envMap[e.Name] = e.Value
	}

	// CAPABILITY must be pack-deploy.
	if v := envMap["CAPABILITY"]; v != "pack-deploy" {
		t.Errorf("CAPABILITY=%q, want pack-deploy", v)
	}

	// CLUSTER_REF identifies the target cluster.
	if v := envMap["CLUSTER_REF"]; v != pe.Spec.TargetClusterRef {
		t.Errorf("CLUSTER_REF=%q, want %q", v, pe.Spec.TargetClusterRef)
	}

	// PACK_REGISTRY_REF = {URL}@{Digest} — constructed from ClusterPack.Spec.RegistryRef.
	expectedRef := "registry.ontai.dev/packs/" + cpName + "@sha256:abc123"
	if v := envMap["PACK_REGISTRY_REF"]; v != expectedRef {
		t.Errorf("PACK_REGISTRY_REF=%q, want %q", v, expectedRef)
	}

	// PACK_CHECKSUM from ClusterPack.Spec.Checksum.
	if v := envMap["PACK_CHECKSUM"]; v != "sha256:def456" {
		t.Errorf("PACK_CHECKSUM=%q, want sha256:def456", v)
	}

	// PACK_SIGNATURE from ClusterPack.Status.PackSignature.
	if v := envMap["PACK_SIGNATURE"]; v != "valid-sig==" {
		t.Errorf("PACK_SIGNATURE=%q, want valid-sig==", v)
	}

	// OPERATION_RESULT_CM is set to the deterministic result CM name.
	expectedCM := "pack-deploy-result-" + pe.Name
	if v := envMap["OPERATION_RESULT_CM"]; v != expectedCM {
		t.Errorf("OPERATION_RESULT_CM=%q, want %q", v, expectedCM)
	}
}

// TestJobSubmission_CredentialsVolumeMount verifies that the pack-deploy Job mounts
// the target cluster kubeconfig Secret. The conductor executor uses this to apply
// rendered manifests to the target cluster. wrapper-design.md §6.
func TestJobSubmission_CredentialsVolumeMount(t *testing.T) {
	fakeClient, pe := jobSubmissionSetup(t)

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: record.NewFakeRecorder(32),
	}

	reconcilePackExecution(t, r, pe.Name, pe.Namespace)

	ctx := context.Background()
	job := &batchv1.Job{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: packDeployJobName(pe.Name), Namespace: pe.Namespace}, job); err != nil {
		t.Fatalf("Job not found: %v", err)
	}

	// Verify kubeconfig Secret volume exists.
	var kubeconfigVolume *batchv1.Job
	_ = kubeconfigVolume
	volumes := job.Spec.Template.Spec.Volumes
	found := false
	for _, v := range volumes {
		if v.Name == "kubeconfig" {
			found = true
			if v.Secret == nil {
				t.Error("kubeconfig volume has no Secret source")
			} else if v.Secret.SecretName != "target-cluster-kubeconfig" {
				t.Errorf("kubeconfig Secret name=%q, want target-cluster-kubeconfig", v.Secret.SecretName)
			}
			break
		}
	}
	if !found {
		t.Error("Job missing kubeconfig volume")
	}

	// Verify volume is mounted in the container at /var/run/secrets/kubeconfig.
	containers := job.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		t.Fatal("no containers in Job")
	}
	mountFound := false
	for _, m := range containers[0].VolumeMounts {
		if m.Name == "kubeconfig" {
			mountFound = true
			if m.MountPath != "/var/run/secrets/kubeconfig" {
				t.Errorf("kubeconfig MountPath=%q, want /var/run/secrets/kubeconfig", m.MountPath)
			}
			if !m.ReadOnly {
				t.Error("kubeconfig mount must be ReadOnly=true")
			}
			break
		}
	}
	if !mountFound {
		t.Error("kubeconfig volume not mounted in container")
	}
}

// TestJobSubmission_NoDAGFields verifies that the submitted Job is a standard
// single Job with no DAG-specific fields. Wrapper submits one Job per PackExecution,
// never a multi-Job DAG. wrapper-design.md §4.
func TestJobSubmission_NoDAGFields(t *testing.T) {
	fakeClient, pe := jobSubmissionSetup(t)

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: record.NewFakeRecorder(32),
	}

	reconcilePackExecution(t, r, pe.Name, pe.Namespace)

	ctx := context.Background()
	job := &batchv1.Job{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: packDeployJobName(pe.Name), Namespace: pe.Namespace}, job); err != nil {
		t.Fatalf("Job not found: %v", err)
	}

	// A single pack-deploy Job has exactly one container.
	if n := len(job.Spec.Template.Spec.Containers); n != 1 {
		t.Errorf("expected 1 container (no DAG sidecar/init), got %d", n)
	}

	// No init containers — all logic is in the single conductor container.
	if n := len(job.Spec.Template.Spec.InitContainers); n != 0 {
		t.Errorf("expected 0 init containers (no DAG initializer), got %d", n)
	}

	// Parallelism is not set (nil or 0) — single execution.
	if job.Spec.Parallelism != nil && *job.Spec.Parallelism > 1 {
		t.Errorf("Parallelism=%d, want nil or 1 (single Job)", *job.Spec.Parallelism)
	}

	// RestartPolicy must be Never — no auto-retry. Failure surfaces to the human.
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != "Never" {
		t.Errorf("RestartPolicy=%q, want Never", pod.RestartPolicy)
	}
}

// TestJobFailed_PackExecutionFailed verifies that when the pack-deploy Job reports
// Status.Failed > 0, the PackExecutionReconciler sets PackExecutionFailed=True
// and returns without requeue (human intervention required). No automatic retry.
func TestJobFailed_PackExecutionFailed(t *testing.T) {
	const (
		peName     = "pe-job-fail"
		cpName     = "my-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-h"
		profileRef = "profile-h"
	)

	fakeClient, pe := allGatesSetup(t, peName, cpName, cpVersion, clusterRef, profileRef)
	ctx := context.Background()

	// Pre-create the Job in a Failed state (conductor executor failed).
	failedJob := newJob(packDeployJobName(peName), "infra-system", 0, 1)
	if err := fakeClient.Create(ctx, failedJob); err != nil {
		t.Fatalf("create failed Job: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: record.NewFakeRecorder(32),
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")

	// No requeue — failed state requires human investigation and intervention.
	if result.RequeueAfter != 0 || result.Requeue {
		t.Errorf("expected no requeue after Job failure (human intervention required), got %+v", result)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}

	// PackExecutionFailed=True with JobFailed reason.
	failedCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionFailed)
	if failedCond == nil {
		t.Fatal("PackExecutionFailed condition not set after Job failure")
	}
	if failedCond.Status != metav1.ConditionTrue {
		t.Errorf("PackExecutionFailed=%s, want True", failedCond.Status)
	}
	if failedCond.Reason != infrav1alpha1.ReasonJobFailed {
		t.Errorf("PackExecutionFailed reason=%q, want %q", failedCond.Reason, infrav1alpha1.ReasonJobFailed)
	}

	// Running condition must not be True.
	runningCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionRunning)
	if runningCond != nil && runningCond.Status == metav1.ConditionTrue {
		t.Error("Running must not be True when Job has failed")
	}

	// Succeeded must not be True.
	succeededCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionSucceeded)
	if succeededCond != nil && succeededCond.Status == metav1.ConditionTrue {
		t.Error("Succeeded must not be True when Job has failed")
	}
}

// TestJobSucceeded_PackExecutionSucceeded verifies that when the pack-deploy Job
// reports Status.Succeeded > 0 and the OperationResult ConfigMap exists:
//   - PackExecution Succeeded=True
//   - OperationResultRef is set
//   - PackInstance is created
func TestJobSucceeded_PackExecutionSucceeded(t *testing.T) {
	const (
		peName     = "pe-job-ok"
		cpName     = "my-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-i"
		profileRef = "profile-i"
	)

	fakeClient, pe := allGatesSetup(t, peName, cpName, cpVersion, clusterRef, profileRef)
	ctx := context.Background()

	// Pre-create succeeded Job and OperationResult ConfigMap.
	succeededJob := newJob(packDeployJobName(peName), "infra-system", 1, 0)
	cm := newOperationResultCM(peName, "infra-system")
	if err := fakeClient.Create(ctx, succeededJob); err != nil {
		t.Fatalf("create succeeded Job: %v", err)
	}
	if err := fakeClient.Create(ctx, cm); err != nil {
		t.Fatalf("create OperationResult CM: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: record.NewFakeRecorder(32),
	}

	result := reconcilePackExecution(t, r, peName, "infra-system")

	// Terminal success — no requeue.
	if result.RequeueAfter != 0 || result.Requeue {
		t.Errorf("expected no requeue after success, got %+v", result)
	}

	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}

	// Succeeded=True.
	succeededCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionSucceeded)
	if succeededCond == nil || succeededCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Succeeded=True, got %+v", succeededCond)
	}

	// OperationResultRef set.
	expectedRef := "pack-deploy-result-" + peName
	if updated.Status.OperationResultRef != expectedRef {
		t.Errorf("OperationResultRef=%q, want %q", updated.Status.OperationResultRef, expectedRef)
	}

	// PackInstance created with name {cpName}-{clusterRef}.
	piName := cpName + "-" + clusterRef
	pi := &infrav1alpha1.PackInstance{}
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: piName, Namespace: "infra-system"}, pi); err != nil {
		t.Fatalf("PackInstance %q not created after Job success: %v", piName, err)
	}
	if pi.Spec.ClusterPackRef != cpName {
		t.Errorf("PackInstance.Spec.ClusterPackRef=%q, want %q", pi.Spec.ClusterPackRef, cpName)
	}
	if pi.Spec.TargetClusterRef != clusterRef {
		t.Errorf("PackInstance.Spec.TargetClusterRef=%q, want %q", pi.Spec.TargetClusterRef, clusterRef)
	}
	if len(pi.OwnerReferences) != 1 || pi.OwnerReferences[0].Kind != "PackExecution" {
		t.Errorf("PackInstance ownerRef not pointing to PackExecution: %+v", pi.OwnerReferences)
	}
}

// TestIdempotency_RunningJob_NoNewJob verifies that when a pack-deploy Job is already
// running (Status.Succeeded=0, Status.Failed=0), a second reconcile does not create
// a duplicate Job. PackExecution Running=True and reconcile requeues after 10s.
func TestIdempotency_RunningJob_NoNewJob(t *testing.T) {
	const (
		peName     = "pe-idem"
		cpName     = "my-pack"
		cpVersion  = "v1.0.0"
		clusterRef = "cluster-j"
		profileRef = "profile-j"
	)

	fakeClient, pe := allGatesSetup(t, peName, cpName, cpVersion, clusterRef, profileRef)
	ctx := context.Background()

	// Pre-create a running Job (not yet Succeeded or Failed).
	runningJob := newJob(packDeployJobName(peName), "infra-system", 0, 0)
	if err := fakeClient.Create(ctx, runningJob); err != nil {
		t.Fatalf("create running Job: %v", err)
	}

	r := &controller.PackExecutionReconciler{
		Client:   fakeClient,
		Scheme:   buildTestScheme(t),
		Recorder: record.NewFakeRecorder(32),
	}

	// First reconcile with running Job — should set Running=True, requeue 10s.
	result1 := reconcilePackExecution(t, r, peName, "infra-system")
	if result1.RequeueAfter == 0 {
		t.Error("expected requeue when Job is running, got RequeueAfter=0")
	}

	// Second reconcile — Job still running. Must not create a duplicate Job.
	result2 := reconcilePackExecution(t, r, peName, "infra-system")
	if result2.RequeueAfter == 0 {
		t.Error("expected requeue on second reconcile with running Job")
	}

	// Exactly one Job must exist.
	jobList := &batchv1.JobList{}
	if err := fakeClient.List(ctx, jobList, client.InNamespace("infra-system")); err != nil {
		t.Fatalf("list Jobs: %v", err)
	}
	if len(jobList.Items) != 1 {
		t.Errorf("expected exactly 1 Job (no duplicate), got %d", len(jobList.Items))
	}

	// PackExecution Running=True.
	updated := &infrav1alpha1.PackExecution{}
	if err := fakeClient.Get(ctx, client.ObjectKeyFromObject(pe), updated); err != nil {
		t.Fatalf("get PackExecution: %v", err)
	}
	runningCond := infrav1alpha1.FindCondition(updated.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionRunning)
	if runningCond == nil || runningCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Running=True while Job is running, got %+v", runningCond)
	}
}
