package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
)

const (
	// packDeployCapability is the conductor capability name for pack deployment.
	// This value must match the capability declared in conductor-schema.md.
	packDeployCapability = "pack-deploy"

	// kueueQueueLabel is the Kueue admission label. All pack-deploy Jobs MUST
	// carry this label. Direct Job creation without Kueue admission is an
	// invariant violation. wrapper-design.md §4.
	kueueQueueLabel = "kueue.x-k8s.io/queue-name"

	// packDeployQueue is the Kueue LocalQueue name in infra-system that admits
	// pack-deploy Jobs. This queue is provisioned by Guardian from QueueProfile.
	packDeployQueue = "pack-deploy-queue"

	// operationResultCMPrefix is used to build the ConfigMap name for OperationResult.
	operationResultCMPrefix = "pack-deploy-result-"

	// packDeployJobTTL is the TTL seconds after a pack-deploy Job finishes.
	// The Job is deleted 600 seconds after completion. wrapper-design.md §4.
	packDeployJobTTL = int32(600)

	// signatureRequeueInterval is the requeue interval when awaiting conductor signature.
	signatureRequeueInterval = 15 * time.Second

	// gateRequeueInterval is the requeue interval when PermissionSnapshot or
	// RBACProfile gates have not yet cleared.
	gateRequeueInterval = 30 * time.Second

	// wrapperRunnerServiceAccount is the ServiceAccount used by pack-deploy Jobs.
	// wrapper-design.md §4.
	wrapperRunnerServiceAccount = "wrapper-runner"

	// wrapperRunnerNamespace is the namespace where the runner ServiceAccount lives.
	// Conductor execute-mode Jobs submitted by Wrapper run in ont-system.
	wrapperRunnerNamespace = "ont-system"

	// conductorImageEnvVar is the env var the operator reads for the conductor image.
	// Defaults to the local dev registry if not set.
	conductorImageDefault = "10.20.0.1:5000/ontai-dev/conductor:dev"

	// kubeconfigSecretName is the kubeconfig Secret in ont-system mounted by pack-deploy Jobs.
	kubeconfigSecretName = "target-cluster-kubeconfig"

	// kubeconfigSecretNamespace is the namespace where kubeconfig Secrets live.
	kubeconfigSecretNamespace = "ont-system"
)

// PackExecutionReconciler watches PackExecution CRs and manages the 5-gate check
// and pack-deploy Job lifecycle.
//
// Reconcile loop:
//  1. Fetch PackExecution CR. Not found → no-op (INV-006).
//  2. Defer status patch.
//  3. Advance ObservedGeneration.
//  4. Initialize LineageSynced on first observation.
//  5. If Succeeded=True, stop (terminal state).
//  6. If PackRevoked=True, stop (no requeue, human intervention required).
//  7. Run 5-gate check in order:
//     gate 0. ConductorReady gate — target cluster TalosCluster.ConductorReady=True
//     gate 1. Signature gate — ClusterPack.status.Signed=true
//     gate 2. Revocation gate — ClusterPack conditions Revoked != True
//     gate 3. PermissionSnapshot gate — target cluster snapshot current
//     gate 4. RBACProfile gate — admissionProfileRef provisioned=true
//  8. Check for existing Job; if running, update Running condition and requeue.
//  9. Read OperationResult ConfigMap; if present, mark Succeeded.
//  10. Submit pack-deploy Job via Kueue.
//
// Gate 0 is checked first because ConductorReady is a cluster-level prerequisite,
// not a pack-level prerequisite. A cluster without an Available Conductor cannot
// safely receive any pack delivery regardless of pack signature or RBAC state.
// platform-schema.md §12, Gap 27.
type PackExecutionReconciler struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile is the main reconciliation loop for PackExecution.
//
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=packexecutions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=packexecutions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=packexecutions/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=clusterpacks,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
func (r *PackExecutionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Step A — Fetch the PackExecution CR.
	pe := &infrav1alpha1.PackExecution{}
	if err := r.Client.Get(ctx, req.NamespacedName, pe); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("PackExecution not found — likely deleted, ignoring",
				"namespacedName", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PackExecution %s: %w", req.NamespacedName, err)
	}

	// Step B — Deferred status patch.
	patchBase := client.MergeFrom(pe.DeepCopy())
	defer func() {
		if err := r.Client.Status().Patch(ctx, pe, patchBase); err != nil {
			if !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to patch PackExecution status",
					"name", pe.Name, "namespace", pe.Namespace)
			}
		}
	}()

	// Step C — Advance ObservedGeneration.
	pe.Status.ObservedGeneration = pe.Generation

	// Step D — Initialize LineageSynced on first observation (one-time write).
	if infrav1alpha1.FindCondition(pe.Status.Conditions, infrav1alpha1.ConditionTypeLineageSynced) == nil {
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypeLineageSynced,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonLineageControllerAbsent,
			"InfrastructureLineageController is not yet deployed.",
			pe.Generation,
		)
	}

	// Step E — Terminal state guard: Succeeded.
	succeededCond := infrav1alpha1.FindCondition(pe.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionSucceeded)
	if succeededCond != nil && succeededCond.Status == metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	// Step F — Terminal state guard: PackRevoked (no requeue).
	revokedCond := infrav1alpha1.FindCondition(pe.Status.Conditions, infrav1alpha1.ConditionTypePackRevoked)
	if revokedCond != nil && revokedCond.Status == metav1.ConditionTrue {
		logger.Info("PackExecution blocked — ClusterPack is revoked",
			"name", pe.Name, "namespace", pe.Namespace)
		return ctrl.Result{}, nil
	}

	// Step G — 5-gate check. All gates must pass before Job submission.
	// Gate 0: ConductorReady gate — cluster-level prerequisite checked first.
	// Reads the TalosCluster for the target cluster from seam-tenant-{clusterRef}
	// namespace and verifies that ConductorReady=True. If absent or False, the
	// cluster's Conductor agent is not yet Available and pack delivery is unsafe.
	// platform-schema.md §12 Conductor Deployment Contract. Gap 27.
	conductorReady, err := r.isConductorReadyForCluster(ctx, pe)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check ConductorReady for cluster %s: %w",
			pe.Spec.TargetClusterRef, err)
	}
	if !conductorReady {
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackExecutionWaiting,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonAwaitingConductorReady,
			fmt.Sprintf("Target cluster %q TalosCluster does not yet have ConductorReady=True. "+
				"Waiting for the Conductor agent Deployment to become Available.",
				pe.Spec.TargetClusterRef),
			pe.Generation,
		)
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonGatesClearing,
			"Waiting for ConductorReady gate (gate 0).",
			pe.Generation,
		)
		logger.Info("PackExecution gate 0 (ConductorReady) not cleared — requeueing",
			"name", pe.Name, "cluster", pe.Spec.TargetClusterRef)
		return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
	}
	// Gate 0 cleared — clear the Waiting condition if set.
	infrav1alpha1.SetCondition(
		&pe.Status.Conditions,
		infrav1alpha1.ConditionTypePackExecutionWaiting,
		metav1.ConditionFalse,
		infrav1alpha1.ReasonAwaitingConductorReady,
		"ConductorReady gate cleared.",
		pe.Generation,
	)

	// Gate 1: Signature gate.
	cp := &infrav1alpha1.ClusterPack{}
	cpKey := client.ObjectKey{Name: pe.Spec.ClusterPackRef.Name, Namespace: pe.Namespace}
	if err := r.Client.Get(ctx, cpKey, cp); err != nil {
		if apierrors.IsNotFound(err) {
			infrav1alpha1.SetCondition(
				&pe.Status.Conditions,
				infrav1alpha1.ConditionTypePackExecutionPending,
				metav1.ConditionTrue,
				infrav1alpha1.ReasonGatesClearing,
				fmt.Sprintf("ClusterPack %q not found.", pe.Spec.ClusterPackRef.Name),
				pe.Generation,
			)
			return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ClusterPack %s: %w", cpKey, err)
	}

	// Verify version matches to guard against name reuse.
	if cp.Spec.Version != pe.Spec.ClusterPackRef.Version {
		msg := fmt.Sprintf("ClusterPack version mismatch: expected %q, found %q.",
			pe.Spec.ClusterPackRef.Version, cp.Spec.Version)
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackExecutionFailed,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonJobFailed,
			msg,
			pe.Generation,
		)
		r.Recorder.Event(pe, corev1.EventTypeWarning, "ClusterPackVersionMismatch", msg)
		return ctrl.Result{}, nil
	}

	if !cp.Status.Signed {
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackSignaturePending,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonAwaitingSignature,
			"ClusterPack has not yet been signed by the conductor signing loop.",
			pe.Generation,
		)
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonAwaitingSignature,
			"Waiting for ClusterPack signature.",
			pe.Generation,
		)
		logger.Info("PackExecution gate 1 (signature) not cleared — requeueing",
			"name", pe.Name, "clusterPack", pe.Spec.ClusterPackRef.Name)
		return ctrl.Result{RequeueAfter: signatureRequeueInterval}, nil
	}
	// Signature cleared — clear the SignaturePending condition if set.
	infrav1alpha1.SetCondition(
		&pe.Status.Conditions,
		infrav1alpha1.ConditionTypePackSignaturePending,
		metav1.ConditionFalse,
		infrav1alpha1.ReasonPackSigned,
		"ClusterPack is signed.",
		pe.Generation,
	)

	// Gate 2: Revocation gate.
	cpRevokedCond := infrav1alpha1.FindCondition(cp.Status.Conditions, infrav1alpha1.ConditionTypeClusterPackRevoked)
	if cpRevokedCond != nil && cpRevokedCond.Status == metav1.ConditionTrue {
		msg := fmt.Sprintf("ClusterPack %q has been revoked. Human intervention required.", cp.Name)
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackRevoked,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonClusterPackRevoked,
			msg,
			pe.Generation,
		)
		r.Recorder.Event(pe, corev1.EventTypeWarning, "PackRevoked", msg)
		logger.Error(fmt.Errorf("pack revoked"), msg, "name", pe.Name, "clusterPack", cp.Name)
		return ctrl.Result{}, nil // no requeue — human intervention required
	}

	// Gate 3: PermissionSnapshot gate.
	// Read PermissionSnapshot via unstructured to avoid cross-operator type import.
	// The snapshot is in the same namespace and has the targetClusterRef name.
	psReady, err := r.isPermissionSnapshotCurrent(ctx, pe)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check PermissionSnapshot: %w", err)
	}
	if !psReady {
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePermissionSnapshotOutOfSync,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonSnapshotOutOfSync,
			"PermissionSnapshot for target cluster is not current.",
			pe.Generation,
		)
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonGatesClearing,
			"Waiting for PermissionSnapshot gate.",
			pe.Generation,
		)
		logger.Info("PackExecution gate 3 (PermissionSnapshot) not cleared — requeueing",
			"name", pe.Name)
		return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
	}
	infrav1alpha1.SetCondition(
		&pe.Status.Conditions,
		infrav1alpha1.ConditionTypePermissionSnapshotOutOfSync,
		metav1.ConditionFalse,
		infrav1alpha1.ReasonSnapshotOutOfSync,
		"PermissionSnapshot is current.",
		pe.Generation,
	)

	// Gate 4: RBACProfile gate.
	rbacReady, err := r.isRBACProfileProvisioned(ctx, pe)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check RBACProfile: %w", err)
	}
	if !rbacReady {
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypeRBACProfileNotProvisioned,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonRBACProfileNotReady,
			"RBACProfile has not reached provisioned=true.",
			pe.Generation,
		)
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonGatesClearing,
			"Waiting for RBACProfile gate.",
			pe.Generation,
		)
		logger.Info("PackExecution gate 4 (RBACProfile) not cleared — requeueing",
			"name", pe.Name)
		return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
	}
	infrav1alpha1.SetCondition(
		&pe.Status.Conditions,
		infrav1alpha1.ConditionTypeRBACProfileNotProvisioned,
		metav1.ConditionFalse,
		infrav1alpha1.ReasonRBACProfileNotReady,
		"RBACProfile is provisioned.",
		pe.Generation,
	)

	// All gates cleared.
	infrav1alpha1.SetCondition(
		&pe.Status.Conditions,
		infrav1alpha1.ConditionTypePackExecutionPending,
		metav1.ConditionFalse,
		infrav1alpha1.ReasonJobSubmitted,
		"All gates cleared.",
		pe.Generation,
	)

	// Step H — Check for existing pack-deploy Job.
	jobName := packDeployJobName(pe)
	existingJob := &batchv1.Job{}
	jobKey := client.ObjectKey{Name: jobName, Namespace: pe.Namespace}
	jobExists := false
	if err := r.Client.Get(ctx, jobKey, existingJob); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get Job %s: %w", jobName, err)
		}
	} else {
		jobExists = true
	}

	if jobExists {
		// Check if Job has completed.
		if existingJob.Status.Succeeded > 0 {
			// Step I — Read OperationResult ConfigMap.
			resultCMName := operationResultCMPrefix + pe.Name
			resultCM := &corev1.ConfigMap{}
			resultKey := client.ObjectKey{Name: resultCMName, Namespace: pe.Namespace}
			if err := r.Client.Get(ctx, resultKey, resultCM); err != nil {
				if apierrors.IsNotFound(err) {
					// Job succeeded but ConfigMap not yet written — requeue briefly.
					return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
				}
				return ctrl.Result{}, fmt.Errorf("failed to get OperationResult ConfigMap: %w", err)
			}
			pe.Status.OperationResultRef = resultCMName
			infrav1alpha1.SetCondition(
				&pe.Status.Conditions,
				infrav1alpha1.ConditionTypePackExecutionRunning,
				metav1.ConditionFalse,
				infrav1alpha1.ReasonJobSucceeded,
				"pack-deploy Job completed successfully.",
				pe.Generation,
			)
			infrav1alpha1.SetCondition(
				&pe.Status.Conditions,
				infrav1alpha1.ConditionTypePackExecutionSucceeded,
				metav1.ConditionTrue,
				infrav1alpha1.ReasonJobSucceeded,
				"pack-deploy Job completed and OperationResult written.",
				pe.Generation,
			)
			r.Recorder.Event(pe, corev1.EventTypeNormal, "Succeeded", "pack-deploy Job completed successfully.")
			logger.Info("PackExecution succeeded",
				"name", pe.Name, "jobName", jobName, "resultCM", resultCMName)
			return ctrl.Result{}, nil
		}

		if existingJob.Status.Failed > 0 {
			msg := fmt.Sprintf("pack-deploy Job %q failed. Check Job logs.", jobName)
			infrav1alpha1.SetCondition(
				&pe.Status.Conditions,
				infrav1alpha1.ConditionTypePackExecutionRunning,
				metav1.ConditionFalse,
				infrav1alpha1.ReasonJobFailed,
				msg,
				pe.Generation,
			)
			infrav1alpha1.SetCondition(
				&pe.Status.Conditions,
				infrav1alpha1.ConditionTypePackExecutionFailed,
				metav1.ConditionTrue,
				infrav1alpha1.ReasonJobFailed,
				msg,
				pe.Generation,
			)
			r.Recorder.Event(pe, corev1.EventTypeWarning, "JobFailed", msg)
			logger.Error(fmt.Errorf("job failed"), msg, "name", pe.Name, "jobName", jobName)
			return ctrl.Result{}, nil
		}

		// Job exists and is still running.
		infrav1alpha1.SetCondition(
			&pe.Status.Conditions,
			infrav1alpha1.ConditionTypePackExecutionRunning,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonJobSubmitted,
			"pack-deploy Job is running.",
			pe.Generation,
		)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Step J — Submit pack-deploy Job via Kueue. Direct Job creation without
	// Kueue admission is an invariant violation. wrapper-design.md §4.
	job := r.buildPackDeployJob(pe, cp, jobName)
	if err := r.Client.Create(ctx, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create pack-deploy Job %s: %w", jobName, err)
	}
	pe.Status.JobName = jobName
	infrav1alpha1.SetCondition(
		&pe.Status.Conditions,
		infrav1alpha1.ConditionTypePackExecutionRunning,
		metav1.ConditionTrue,
		infrav1alpha1.ReasonJobSubmitted,
		fmt.Sprintf("pack-deploy Job %q submitted to Kueue.", jobName),
		pe.Generation,
	)
	r.Recorder.Event(pe, corev1.EventTypeNormal, "JobSubmitted",
		fmt.Sprintf("pack-deploy Job %q submitted.", jobName))
	logger.Info("PackExecution pack-deploy Job submitted",
		"name", pe.Name, "jobName", jobName)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// isPermissionSnapshotCurrent reads the PermissionSnapshot for the target cluster
// via unstructured to avoid importing guardian types. Returns true if the snapshot
// has a Current=True condition. Returns false (not error) if the snapshot is not
// found or not current.
func (r *PackExecutionReconciler) isPermissionSnapshotCurrent(ctx context.Context, pe *infrav1alpha1.PackExecution) (bool, error) {
	ps := &unstructured.Unstructured{}
	ps.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "PermissionSnapshot",
	})
	psKey := types.NamespacedName{
		Name:      pe.Spec.TargetClusterRef,
		Namespace: pe.Namespace,
	}
	if err := r.Client.Get(ctx, psKey, ps); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	conditions, found, err := unstructured.NestedSlice(ps.Object, "status", "conditions")
	if err != nil || !found {
		return false, nil
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "Current" && cond["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}

// isRBACProfileProvisioned reads the RBACProfile referenced by the PackExecution
// via unstructured to avoid importing guardian types. Returns true if the profile
// has provisioned=true in its status.
func (r *PackExecutionReconciler) isRBACProfileProvisioned(ctx context.Context, pe *infrav1alpha1.PackExecution) (bool, error) {
	rp := &unstructured.Unstructured{}
	rp.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RBACProfile",
	})
	rpKey := types.NamespacedName{
		Name:      pe.Spec.AdmissionProfileRef,
		Namespace: pe.Namespace,
	}
	if err := r.Client.Get(ctx, rpKey, rp); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	provisioned, found, err := unstructured.NestedBool(rp.Object, "status", "provisioned")
	if err != nil || !found {
		return false, nil
	}
	return provisioned, nil
}

// isConductorReadyForCluster reads the TalosCluster CR for the target cluster via
// unstructured (to avoid importing platform types) and checks whether the
// ConductorReady condition has status=True. Returns (true, nil) when ConductorReady=True,
// (false, nil) when the TalosCluster is not found, the condition is absent, or the
// condition is False. Returns (false, err) only for unexpected API errors.
//
// The TalosCluster for a target cluster lives in seam-tenant-{clusterRef} namespace.
// platform-schema.md §12, Gap 27.
func (r *PackExecutionReconciler) isConductorReadyForCluster(ctx context.Context, pe *infrav1alpha1.PackExecution) (bool, error) {
	tenantNS := "seam-tenant-" + pe.Spec.TargetClusterRef
	tc := &unstructured.Unstructured{}
	tc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "platform.ontai.dev",
		Version: "v1alpha1",
		Kind:    "TalosCluster",
	})
	tcKey := types.NamespacedName{
		Name:      pe.Spec.TargetClusterRef,
		Namespace: tenantNS,
	}
	if err := r.Client.Get(ctx, tcKey, tc); err != nil {
		if apierrors.IsNotFound(err) {
			// TalosCluster not yet present — not an error, not ready.
			return false, nil
		}
		return false, err
	}

	// Walk status.conditions looking for ConductorReady=True.
	conditions, found, err := unstructured.NestedSlice(tc.Object, "status", "conditions")
	if err != nil || !found {
		return false, nil
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "ConductorReady" && cond["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}

// buildPackDeployJob constructs the pack-deploy Job spec for Kueue admission.
// The Job carries the kueue.x-k8s.io/queue-name label — without this label,
// the Job will not be admitted. wrapper-design.md §4.
func (r *PackExecutionReconciler) buildPackDeployJob(
	pe *infrav1alpha1.PackExecution,
	cp *infrav1alpha1.ClusterPack,
	jobName string,
) *batchv1.Job {
	ttl := packDeployJobTTL
	conductorImage := conductorImageDefault

	resultCMName := operationResultCMPrefix + pe.Name

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: pe.Namespace,
			Labels: map[string]string{
				kueueQueueLabel:              packDeployQueue,
				"app.kubernetes.io/part-of": "wrapper",
				"infra.ontai.dev/pe-name":   pe.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         infrav1alpha1.GroupVersion.String(),
					Kind:               "PackExecution",
					Name:               pe.Name,
					UID:                pe.UID,
					Controller:         boolPtr(true),
					BlockOwnerDeletion: boolPtr(true),
				},
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: wrapperRunnerServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{
						{
							Name: "kubeconfig",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: kubeconfigSecretName,
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "conductor",
							Image: conductorImage,
							Env: []corev1.EnvVar{
								{Name: "CAPABILITY", Value: packDeployCapability},
								{Name: "CLUSTER_REF", Value: pe.Spec.TargetClusterRef},
								{Name: "OPERATION_RESULT_CM", Value: resultCMName},
								{Name: "PACK_REGISTRY_REF", Value: cp.Spec.RegistryRef.URL + "@" + cp.Spec.RegistryRef.Digest},
								{Name: "PACK_CHECKSUM", Value: cp.Spec.Checksum},
								{Name: "PACK_SIGNATURE", Value: cp.Status.PackSignature},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "kubeconfig",
									MountPath: "/var/run/secrets/kubeconfig",
									ReadOnly:  true,
								},
							},
						},
					},
				},
			},
		},
	}
	return job
}

// packDeployJobName constructs a deterministic Job name from the PackExecution name.
func packDeployJobName(pe *infrav1alpha1.PackExecution) string {
	return fmt.Sprintf("pack-deploy-%s", pe.Name)
}

func boolPtr(b bool) *bool { return &b }

// SetupWithManager registers PackExecutionReconciler as the controller for PackExecution.
func (r *PackExecutionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.PackExecution{}).
		Owns(&batchv1.Job{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
