package controller

import (
	"context"
	"fmt"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

	// packDeployQueue is the Kueue LocalQueue name that admits pack-deploy Jobs.
	// The queue lives in the same namespace as the PackExecution (seam-tenant-{cluster}).
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
	// The ServiceAccount must exist in the Job's namespace (seam-tenant-{clusterRef}).
	// wrapper-design.md §4.
	wrapperRunnerServiceAccount = "wrapper-runner"

	// conductorImageEnvVar is the env var the operator reads for the conductor image.
	// Defaults to the local dev registry if not set.
	conductorImageDefault = "10.20.0.1:5000/ontai-dev/conductor-execute:dev"

	// kubeconfigSecretName is the name of the kubeconfig Secret mounted by pack-deploy Jobs.
	// The Secret lives in the Job's own namespace (seam-tenant-{clusterRef}) — Platform
	// copies it there from seam-system as part of cluster onboarding. WS4.
	kubeconfigSecretName = "target-cluster-kubeconfig"
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
//     gate 0. ConductorReady gate — RunnerConfig in ont-system has ≥1 capability
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
	// Verifies the TalosCluster is registered (seam-tenant-{clusterRef} or
	// seam-system) and that the RunnerConfig for the cluster exists in ont-system
	// with at least one published capability. The published capability list is the
	// correct signal that the Conductor agent is live and ready to accept Jobs.
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
			fmt.Sprintf("Conductor agent for cluster %q is not yet ready: "+
				"RunnerConfig in ont-system has no published capabilities. "+
				"Waiting for the Conductor Deployment to start and declare its capability manifest.",
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

			// Fix 3 — Attach Job OwnerReference to OperationResult ConfigMap so it
			// is garbage-collected when the Job TTL expires. Idempotent: skips if the
			// reference is already present. Non-fatal if patching fails.
			jobOwnerRef := metav1.OwnerReference{
				APIVersion:         "batch/v1",
				Kind:               "Job",
				Name:               existingJob.Name,
				UID:                existingJob.UID,
				Controller:         boolPtr(false),
				BlockOwnerDeletion: boolPtr(true),
			}
			alreadyOwned := false
			for _, ref := range resultCM.OwnerReferences {
				if ref.UID == existingJob.UID {
					alreadyOwned = true
					break
				}
			}
			if !alreadyOwned {
				cmPatch := client.MergeFrom(resultCM.DeepCopy())
				resultCM.OwnerReferences = append(resultCM.OwnerReferences, jobOwnerRef)
				if patchErr := r.Client.Patch(ctx, resultCM, cmPatch); patchErr != nil {
					logger.Error(patchErr, "failed to patch OperationResult ConfigMap with Job owner reference",
						"cmName", resultCMName)
				}
			}

			// Step I.b — Create PackInstance in seam-tenant-{clusterRef} to record the
			// delivered pack state. Namespace is explicit per wrapper-schema.md §9.
			// Labels infra.ontai.dev/pack and platform.ontai.dev/cluster enable
			// conductor and tooling to filter PackInstances by pack or cluster.
			// One PackInstance per (ClusterPack, TargetCluster). Idempotent.
			piName := pe.Spec.ClusterPackRef.Name + "-" + pe.Spec.TargetClusterRef
			piNamespace := "seam-tenant-" + pe.Spec.TargetClusterRef
			pi := &infrav1alpha1.PackInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      piName,
					Namespace: piNamespace,
					Labels: map[string]string{
						"infra.ontai.dev/pack":        pe.Spec.ClusterPackRef.Name,
						"platform.ontai.dev/cluster":  pe.Spec.TargetClusterRef,
					},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         infrav1alpha1.GroupVersion.String(),
						Kind:               "PackExecution",
						Name:               pe.Name,
						UID:                pe.UID,
						Controller:         boolPtr(true),
						BlockOwnerDeletion: boolPtr(true),
					}},
				},
				Spec: infrav1alpha1.PackInstanceSpec{
					ClusterPackRef:   pe.Spec.ClusterPackRef.Name,
					Version:          cp.Spec.Version, // WS6: carry version for DSNSReconciler
					TargetClusterRef: pe.Spec.TargetClusterRef,
				},
			}
			if err := r.Client.Create(ctx, pi); err != nil && !apierrors.IsAlreadyExists(err) {
				return ctrl.Result{}, fmt.Errorf("failed to create PackInstance %s: %w", piName, err)
			}
			logger.Info("PackInstance created or already exists",
				"name", piName, "namespace", piNamespace)
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
// has a Fresh=True condition. PermissionSnapshots live exclusively in seam-system
// and are named "snapshot-{clusterRef}". guardian-schema.md §PS condition vocabulary.
// Returns false (not error) if the snapshot is not found or not fresh.
func (r *PackExecutionReconciler) isPermissionSnapshotCurrent(ctx context.Context, pe *infrav1alpha1.PackExecution) (bool, error) {
	ps := &unstructured.Unstructured{}
	ps.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "PermissionSnapshot",
	})
	psKey := types.NamespacedName{
		Name:      "snapshot-" + pe.Spec.TargetClusterRef,
		Namespace: "seam-system",
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
		if cond["type"] == "Fresh" && cond["status"] == "True" {
			return true, nil
		}
	}
	return false, nil
}

// isRBACProfileProvisioned reads the RBACProfile referenced by the PackExecution
// via unstructured to avoid importing guardian types. Returns true if the profile
// has provisioned=true in its status. RBACProfiles live exclusively in seam-system.
// guardian-schema.md §RBACProfile.
func (r *PackExecutionReconciler) isRBACProfileProvisioned(ctx context.Context, pe *infrav1alpha1.PackExecution) (bool, error) {
	rp := &unstructured.Unstructured{}
	rp.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RBACProfile",
	})
	rpKey := types.NamespacedName{
		Name:      pe.Spec.AdmissionProfileRef,
		Namespace: "seam-system",
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

// isConductorReadyForCluster determines whether the Conductor agent for the
// target cluster is live and ready to accept Jobs. It performs two lookups:
//
//  1. TalosCluster namespace (Fix 1): tries seam-tenant-{clusterRef} first; if
//     not found, falls back to seam-system. The management cluster TalosCluster
//     lives in seam-system, tenant cluster TalosCluster in seam-tenant-{name}.
//     If the TalosCluster is absent in both namespaces, the cluster is not yet
//     registered — return false.
//
//  2. RunnerConfig readiness (Fix 2): looks up the RunnerConfig named
//     {targetClusterRef} in ont-system and checks status.capabilities. A
//     non-empty capabilities list proves the Conductor agent has started, completed
//     leader election, and published its capability manifest. The TalosCluster
//     ConductorReady condition is NOT used — Platform never sets it.
//     conductor-schema.md §5, §10 step 3.
//
// Returns (true, nil) when RunnerConfig exists with ≥1 capability.
// Returns (false, nil) when TalosCluster is absent, RunnerConfig is absent, or
// status.capabilities is empty. Returns (false, err) for unexpected API errors.
// platform-schema.md §12, Gap 27.
func (r *PackExecutionReconciler) isConductorReadyForCluster(ctx context.Context, pe *infrav1alpha1.PackExecution) (bool, error) {
	clusterRef := pe.Spec.TargetClusterRef
	tcGVK := schema.GroupVersionKind{
		Group:   "platform.ontai.dev",
		Version: "v1alpha1",
		Kind:    "TalosCluster",
	}

	// Fix 1: try seam-tenant-{clusterRef} then fall back to seam-system.
	tcFound := false
	for _, ns := range []string{"seam-tenant-" + clusterRef, "seam-system"} {
		tc := &unstructured.Unstructured{}
		tc.SetGroupVersionKind(tcGVK)
		if err := r.Client.Get(ctx, types.NamespacedName{Name: clusterRef, Namespace: ns}, tc); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		tcFound = true
		break
	}
	if !tcFound {
		// TalosCluster absent in both namespaces — cluster not yet registered.
		return false, nil
	}

	// Fix 2: check RunnerConfig in ont-system instead of TalosCluster ConductorReady
	// condition. A RunnerConfig with at least one published capability is the
	// correct signal that Conductor is live. conductor-schema.md §5, §10 step 3.
	rc := &unstructured.Unstructured{}
	rc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "runner.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RunnerConfig",
	})
	if err := r.Client.Get(ctx, types.NamespacedName{Name: clusterRef, Namespace: "ont-system"}, rc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	caps, found, err := unstructured.NestedSlice(rc.Object, "status", "capabilities")
	if err != nil || !found || len(caps) == 0 {
		return false, nil
	}
	return true, nil
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
			Completions:             int32Ptr(1),
			BackoffLimit:            int32Ptr(0),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: wrapperRunnerServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
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
					ImagePullPolicy: corev1.PullAlways,
							Env: []corev1.EnvVar{
								{Name: "CAPABILITY", Value: packDeployCapability},
								{Name: "CLUSTER_REF", Value: pe.Spec.TargetClusterRef},
								{Name: "OPERATION_RESULT_CM", Value: resultCMName},
								{Name: "POD_NAMESPACE", Value: pe.Namespace},
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
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: boolPtr(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								RunAsNonRoot: boolPtr(true),
								SeccompProfile: &corev1.SeccompProfile{
									Type: corev1.SeccompProfileTypeRuntimeDefault,
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

func int32Ptr(i int32) *int32 { return &i }

// SetupWithManager registers PackExecutionReconciler as the controller for PackExecution.
// WS3: Watches PermissionSnapshot and RBACProfile so the reconciler is triggered
// immediately when gates clear, instead of waiting for the 30s gateRequeueInterval.
// GenerationChangedPredicate is scoped to the primary For source only — status-only
// changes on watched objects (which have stable generation) must still trigger.
func (r *PackExecutionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	psObj := &unstructured.Unstructured{}
	psObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "PermissionSnapshot",
	})
	rpObj := &unstructured.Unstructured{}
	rpObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RBACProfile",
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.PackExecution{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.Job{}).
		Watches(psObj, handler.EnqueueRequestsFromMapFunc(r.mapSnapshotToPackExecutions)).
		Watches(rpObj, handler.EnqueueRequestsFromMapFunc(r.mapRBACProfileToPackExecutions)).
		Complete(r)
}

// mapSnapshotToPackExecutions maps a PermissionSnapshot update to the PackExecution
// requests in the corresponding tenant namespace. PermissionSnapshot names follow
// the convention "snapshot-{clusterRef}". Enqueues all PackExecutions in
// seam-tenant-{clusterRef} that reference the same targetClusterRef. WS3.
func (r *PackExecutionReconciler) mapSnapshotToPackExecutions(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	clusterRef := strings.TrimPrefix(obj.GetName(), "snapshot-")
	if clusterRef == obj.GetName() {
		// Not a snapshot-{cluster} name pattern — ignore.
		return nil
	}
	ns := "seam-tenant-" + clusterRef
	peList := &infrav1alpha1.PackExecutionList{}
	if err := r.Client.List(ctx, peList, client.InNamespace(ns)); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, pe := range peList.Items {
		if pe.Spec.TargetClusterRef == clusterRef {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: pe.Name, Namespace: pe.Namespace},
			})
		}
	}
	return requests
}

// mapRBACProfileToPackExecutions maps an RBACProfile update to PackExecution requests
// whose admissionProfileRef matches the profile name. Lists PackExecutions across all
// namespaces since the profile name is the same for all clusters it governs. WS3.
func (r *PackExecutionReconciler) mapRBACProfileToPackExecutions(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	profileName := obj.GetName()
	peList := &infrav1alpha1.PackExecutionList{}
	if err := r.Client.List(ctx, peList); err != nil {
		return nil
	}
	var requests []reconcile.Request
	for _, pe := range peList.Items {
		if pe.Spec.AdmissionProfileRef == profileName {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: pe.Name, Namespace: pe.Namespace},
			})
		}
	}
	return requests
}
