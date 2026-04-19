package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seamv1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/seam-core/pkg/conditions"
	"github.com/ontai-dev/seam-core/pkg/lineage"
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

	// labelPackExecution is the label key used by the single-active-revision POR
	// pattern (conductor T-15). List queries use this label to find the active POR
	// for a given PackExecution. seam-core-schema.md §8.
	labelPackExecution = "ontai.dev/pack-execution"

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
//  9. Read PackOperationResult CR; if present, mark Succeeded.
//  10. Submit pack-deploy Job via Kueue.
//
// Gate 0 is checked first because ConductorReady is a cluster-level prerequisite,
// not a pack-level prerequisite. A cluster without an Available Conductor cannot
// safely receive any pack delivery regardless of pack signature or RBAC state.
// platform-schema.md §12, Gap 27.
// RBACReadyChecker is the function signature for gate 5 RBAC checks.
// In production, the reconciler uses isWrapperRunnerRBACReady (SubjectAccessReview).
// In tests, set RBACChecker to a stub that returns (true, "", nil) to bypass the gate.
type RBACReadyChecker func(ctx context.Context, pe *seamv1alpha1.InfrastructurePackExecution) (bool, string, error)

type PackExecutionReconciler struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Recorder clientevents.EventRecorder
	// RBACChecker overrides gate 5 (SubjectAccessReview) for testing. Nil in production.
	RBACChecker RBACReadyChecker
}

// Reconcile is the main reconciliation loop for PackExecution.
//
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructurepackexecutions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructurepackexecutions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructurepackexecutions/finalizers,verbs=update
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructureclusterpacks,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
func (r *PackExecutionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Step A — Fetch the PackExecution CR.
	pe := &seamv1alpha1.InfrastructurePackExecution{}
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
	if conditions.FindCondition(pe.Status.Conditions, conditions.ConditionTypeLineageSynced) == nil {
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypeLineageSynced,
			metav1.ConditionFalse,
			conditions.ReasonLineageControllerAbsent,
			"InfrastructureLineageController is not yet deployed.",
			pe.Generation,
		)
	}

	// Step E — Terminal state guard: Succeeded.
	succeededCond := conditions.FindCondition(pe.Status.Conditions, conditions.ConditionTypePackExecutionSucceeded)
	if succeededCond != nil && succeededCond.Status == metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	// Step F — Terminal state guard: PackRevoked (no requeue).
	revokedCond := conditions.FindCondition(pe.Status.Conditions, conditions.ConditionTypePackRevoked)
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
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackExecutionWaiting,
			metav1.ConditionTrue,
			conditions.ReasonAwaitingConductorReady,
			fmt.Sprintf("Conductor agent for cluster %q is not yet ready: "+
				"RunnerConfig in ont-system has no published capabilities. "+
				"Waiting for the Conductor Deployment to start and declare its capability manifest.",
				pe.Spec.TargetClusterRef),
			pe.Generation,
		)
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			conditions.ReasonGatesClearing,
			"Waiting for ConductorReady gate (gate 0).",
			pe.Generation,
		)
		logger.Info("PackExecution gate 0 (ConductorReady) not cleared — requeueing",
			"name", pe.Name, "cluster", pe.Spec.TargetClusterRef)
		return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
	}
	// Gate 0 cleared — clear the Waiting condition if set.
	conditions.SetCondition(
		&pe.Status.Conditions,
		conditions.ConditionTypePackExecutionWaiting,
		metav1.ConditionFalse,
		conditions.ReasonAwaitingConductorReady,
		"ConductorReady gate cleared.",
		pe.Generation,
	)

	// Gate 1: Signature gate.
	cp := &seamv1alpha1.InfrastructureClusterPack{}
	cpKey := client.ObjectKey{Name: pe.Spec.ClusterPackRef.Name, Namespace: pe.Namespace}
	if err := r.Client.Get(ctx, cpKey, cp); err != nil {
		if apierrors.IsNotFound(err) {
			conditions.SetCondition(
				&pe.Status.Conditions,
				conditions.ConditionTypePackExecutionPending,
				metav1.ConditionTrue,
				conditions.ReasonGatesClearing,
				fmt.Sprintf("ClusterPack %q not found.", pe.Spec.ClusterPackRef.Name),
				pe.Generation,
			)
			return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ClusterPack %s: %w", cpKey, err)
	}

	// WS4 — Version mismatch: the PackExecution was created against an older
	// ClusterPack version. Delete it so the ClusterPackReconciler can recreate
	// the RunnerConfig with the current version and conductor can create a fresh
	// PackExecution. The deferred status patch will receive a NotFound error on
	// the deleted object, which is already silently ignored.
	if cp.Spec.Version != pe.Spec.ClusterPackRef.Version {
		logger.Info("PackExecution version mismatch — deleting stale PackExecution",
			"name", pe.Name,
			"peVersion", pe.Spec.ClusterPackRef.Version,
			"cpVersion", cp.Spec.Version)
		r.Recorder.Eventf(pe, nil, corev1.EventTypeWarning, "StalePackExecutionDeleted", "StalePackExecutionDeleted",
			"PackExecution %q references ClusterPack version %q but current is %q -- deleting stale object.",
			pe.Name, pe.Spec.ClusterPackRef.Version, cp.Spec.Version)
		if err := r.Client.Delete(ctx, pe); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete stale PackExecution %s: %w", pe.Name, err)
		}
		return ctrl.Result{}, nil
	}

	if !cp.Status.Signed {
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackSignaturePending,
			metav1.ConditionTrue,
			conditions.ReasonAwaitingSignature,
			"ClusterPack has not yet been signed by the conductor signing loop.",
			pe.Generation,
		)
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			conditions.ReasonAwaitingSignature,
			"Waiting for ClusterPack signature.",
			pe.Generation,
		)
		logger.Info("PackExecution gate 1 (signature) not cleared — requeueing",
			"name", pe.Name, "clusterPack", pe.Spec.ClusterPackRef.Name)
		return ctrl.Result{RequeueAfter: signatureRequeueInterval}, nil
	}
	// Signature cleared — clear the SignaturePending condition if set.
	conditions.SetCondition(
		&pe.Status.Conditions,
		conditions.ConditionTypePackSignaturePending,
		metav1.ConditionFalse,
		conditions.ReasonPackSigned,
		"ClusterPack is signed.",
		pe.Generation,
	)

	// Gate 2: Revocation gate.
	cpRevokedCond := conditions.FindCondition(cp.Status.Conditions, conditions.ConditionTypeClusterPackRevoked)
	if cpRevokedCond != nil && cpRevokedCond.Status == metav1.ConditionTrue {
		msg := fmt.Sprintf("ClusterPack %q has been revoked. Human intervention required.", cp.Name)
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackRevoked,
			metav1.ConditionTrue,
			conditions.ReasonClusterPackRevoked,
			msg,
			pe.Generation,
		)
		r.Recorder.Eventf(pe, nil, corev1.EventTypeWarning, "PackRevoked", "PackRevoked", msg)
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
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePermissionSnapshotOutOfSync,
			metav1.ConditionTrue,
			conditions.ReasonSnapshotOutOfSync,
			"PermissionSnapshot for target cluster is not current.",
			pe.Generation,
		)
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			conditions.ReasonGatesClearing,
			"Waiting for PermissionSnapshot gate.",
			pe.Generation,
		)
		logger.Info("PackExecution gate 3 (PermissionSnapshot) not cleared — requeueing",
			"name", pe.Name)
		return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
	}
	conditions.SetCondition(
		&pe.Status.Conditions,
		conditions.ConditionTypePermissionSnapshotOutOfSync,
		metav1.ConditionFalse,
		conditions.ReasonSnapshotOutOfSync,
		"PermissionSnapshot is current.",
		pe.Generation,
	)

	// Gate 4: RBACProfile gate.
	rbacReady, err := r.isRBACProfileProvisioned(ctx, pe)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check RBACProfile: %w", err)
	}
	if !rbacReady {
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypeRBACProfileNotProvisioned,
			metav1.ConditionTrue,
			conditions.ReasonRBACProfileNotReady,
			"RBACProfile has not reached provisioned=true.",
			pe.Generation,
		)
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			conditions.ReasonGatesClearing,
			"Waiting for RBACProfile gate.",
			pe.Generation,
		)
		logger.Info("PackExecution gate 4 (RBACProfile) not cleared — requeueing",
			"name", pe.Name)
		return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
	}
	conditions.SetCondition(
		&pe.Status.Conditions,
		conditions.ConditionTypeRBACProfileNotProvisioned,
		metav1.ConditionFalse,
		conditions.ReasonRBACProfileNotReady,
		"RBACProfile is provisioned.",
		pe.Generation,
	)

	// Gate 5: WrapperRunnerRBAC gate.
	// SubjectAccessReview verifies wrapper-runner SA has required permissions
	// before submitting the Job. Catches stale or missing RBAC before runtime failure.
	rbacCheckFn := r.isWrapperRunnerRBACReady
	if r.RBACChecker != nil {
		rbacCheckFn = r.RBACChecker
	}
	rbacSARReady, rbacSARDenyReason, err := rbacCheckFn(ctx, pe)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check wrapper-runner RBAC: %w", err)
	}
	if !rbacSARReady {
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypeWrapperRunnerRBACNotReady,
			metav1.ConditionTrue,
			conditions.ReasonWrapperRunnerRBACNotReady,
			rbacSARDenyReason,
			pe.Generation,
		)
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackExecutionPending,
			metav1.ConditionTrue,
			conditions.ReasonGatesClearing,
			"Waiting for WrapperRunnerRBAC gate (gate 5).",
			pe.Generation,
		)
		logger.Info("PackExecution gate 5 (WrapperRunnerRBAC) not cleared — requeueing",
			"name", pe.Name, "reason", rbacSARDenyReason)
		return ctrl.Result{RequeueAfter: gateRequeueInterval}, nil
	}
	conditions.SetCondition(
		&pe.Status.Conditions,
		conditions.ConditionTypeWrapperRunnerRBACNotReady,
		metav1.ConditionFalse,
		conditions.ReasonWrapperRunnerRBACNotReady,
		"wrapper-runner has required RBAC permissions.",
		pe.Generation,
	)

	// All gates cleared.
	conditions.SetCondition(
		&pe.Status.Conditions,
		conditions.ConditionTypePackExecutionPending,
		metav1.ConditionFalse,
		conditions.ReasonJobSubmitted,
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
		// Verify the Job belongs to this PackExecution (owner UID match).
		// A stale Job with the same name from a previous PackExecution that has since
		// been deleted may persist until GC catches up. Delete it and requeue so a
		// fresh Job is submitted for the current PackExecution.
		ownedByThis := false
		for _, ref := range existingJob.GetOwnerReferences() {
			if ref.UID == pe.UID {
				ownedByThis = true
				break
			}
		}
		if !ownedByThis {
			logger.Info("found stale Job from previous PackExecution — deleting and requeueing",
				"name", pe.Name, "jobName", jobName)
			if deleteErr := r.Client.Delete(ctx, existingJob); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				return ctrl.Result{}, fmt.Errorf("delete stale Job %s: %w", jobName, deleteErr)
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		jobExists = true
	}

	if jobExists {
		// Check if Job has completed.
		if existingJob.Status.Succeeded > 0 {
			// Step I — Read PackOperationResult CR written by Conductor Job.
			// Single-active-revision pattern (T-15): list by label, pick highest revision.
			resultPOR, err := findLatestPOR(ctx, r.Client, pe.Namespace, pe.Name)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to find PackOperationResult for %q: %w", pe.Name, err)
			}
			if resultPOR == nil {
				// Job succeeded but PackOperationResult not yet written — requeue briefly.
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			pe.Status.OperationResultRef = resultPOR.Name

			// Attach PackExecution OwnerReference so the PackOperationResult is
			// garbage-collected when the PackExecution is deleted. Idempotent.
			alreadyOwned := false
			for _, ref := range resultPOR.OwnerReferences {
				if ref.UID == pe.UID {
					alreadyOwned = true
					break
				}
			}
			if !alreadyOwned {
				porPatch := client.MergeFrom(resultPOR.DeepCopy())
				resultPOR.OwnerReferences = append(resultPOR.OwnerReferences, metav1.OwnerReference{
					APIVersion:         seamv1alpha1.GroupVersion.String(),
					Kind:               "InfrastructurePackExecution",
					Name:               pe.Name,
					UID:                pe.UID,
					Controller:         boolPtr(false),
					BlockOwnerDeletion: boolPtr(true),
				})
				if patchErr := r.Client.Patch(ctx, resultPOR, porPatch); patchErr != nil {
					logger.Error(patchErr, "failed to patch PackOperationResult with PackExecution owner reference",
						"porName", resultPOR.Name)
				}
			}

			// Read result fields directly from typed CR spec. No JSON parsing needed.
			porStatus := resultPOR.Spec.Status
			porFailureReason := resultPOR.Spec.FailureReason
			porDeployedResources := resultPOR.Spec.DeployedResources

			if porStatus == seamv1alpha1.PackResultFailed {
				failMsg := "pack-deploy capability reported failure."
				if porFailureReason != nil {
					failMsg = fmt.Sprintf("step %q failed: %s", porFailureReason.FailedStep, porFailureReason.Reason)
				}
				conditions.SetCondition(
					&pe.Status.Conditions,
					conditions.ConditionTypePackExecutionRunning,
					metav1.ConditionFalse,
					conditions.ReasonJobSucceeded,
					"pack-deploy Job completed.",
					pe.Generation,
				)
				conditions.SetCondition(
					&pe.Status.Conditions,
					conditions.ConditionTypePackExecutionSucceeded,
					metav1.ConditionFalse,
					"CapabilityFailed",
					failMsg,
					pe.Generation,
				)
				r.Recorder.Eventf(pe, nil, corev1.EventTypeWarning, "CapabilityFailed", "CapabilityFailed", failMsg)
				logger.Info("PackExecution capability failed",
					"name", pe.Name, "jobName", jobName, "resultPOR", resultPOR.Name,
					"failedStep", func() string {
						if porFailureReason != nil {
							return porFailureReason.FailedStep
						}
						return ""
					}(),
				)
			} else {
				conditions.SetCondition(
					&pe.Status.Conditions,
					conditions.ConditionTypePackExecutionRunning,
					metav1.ConditionFalse,
					conditions.ReasonJobSucceeded,
					"pack-deploy Job completed successfully.",
					pe.Generation,
				)
				conditions.SetCondition(
					&pe.Status.Conditions,
					conditions.ConditionTypePackExecutionSucceeded,
					metav1.ConditionTrue,
					conditions.ReasonJobSucceeded,
					"pack-deploy Job completed and PackOperationResult written.",
					pe.Generation,
				)
				r.Recorder.Eventf(pe, nil, corev1.EventTypeNormal, "Succeeded", "Succeeded", "pack-deploy Job completed successfully.")
				logger.Info("PackExecution succeeded",
					"name", pe.Name, "jobName", jobName, "resultPOR", resultPOR.Name)
			}

			// Step I.b — Create PackInstance only on capability success.
			// A failed capability means the workload was not (fully) delivered;
			// recording a PackInstance would misrepresent the cluster state.
			if porStatus == seamv1alpha1.PackResultFailed {
				return ctrl.Result{}, nil
			}

			// Create PackInstance in seam-tenant-{clusterRef} to record the
			// delivered pack state. Namespace is explicit per wrapper-schema.md §9.
			// Labels infra.ontai.dev/pack and platform.ontai.dev/cluster enable
			// conductor and tooling to filter PackInstances by pack or cluster.
			// One PackInstance per logical (basePack, TargetCluster). Idempotent.
			// When ClusterPack.spec.basePackName is set, the PackInstance is named
			// {basePackName}-{clusterName} so a newer version supersedes the older
			// one in-place rather than creating a parallel PackInstance. Decision 11.
			piBaseName := cp.Spec.BasePackName
			if piBaseName == "" {
				piBaseName = pe.Spec.ClusterPackRef.Name
			}
			piName := piBaseName + "-" + pe.Spec.TargetClusterRef
			piNamespace := "seam-tenant-" + pe.Spec.TargetClusterRef

			// Determine upgradeDirection by comparing existing PackInstance version.
			upgradeDir := packUpgradeDirection(ctx, r.Client, piName, piNamespace, cp.Spec.Version)

			pi := &seamv1alpha1.InfrastructurePackInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      piName,
					Namespace: piNamespace,
					Labels: map[string]string{
						"infrastructure.ontai.dev/pack":    pe.Spec.ClusterPackRef.Name,
						"infrastructure.ontai.dev/cluster": pe.Spec.TargetClusterRef,
					},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion:         seamv1alpha1.GroupVersion.String(),
						Kind:               "InfrastructurePackExecution",
						Name:               pe.Name,
						UID:                pe.UID,
						Controller:         boolPtr(true),
						BlockOwnerDeletion: boolPtr(true),
					}},
				},
				Spec: seamv1alpha1.InfrastructurePackInstanceSpec{
					ClusterPackRef:   pe.Spec.ClusterPackRef.Name,
					Version:          cp.Spec.Version, // WS6: carry version for DSNSReconciler
					TargetClusterRef: pe.Spec.TargetClusterRef,
				},
			}
			// Wire descendant lineage so the DescendantReconciler can append this
			// PackInstance to the PackExecution's ILI. seam-core-schema.md §3.
			lineage.SetDescendantLabels(pi, lineage.IndexName("PackExecution", pe.Name), pe.Namespace, "wrapper", lineage.PackExecution, pe.GetAnnotations()[lineage.AnnotationDeclaringPrincipal])
			if err := r.Client.Create(ctx, pi); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return ctrl.Result{}, fmt.Errorf("failed to create PackInstance %s: %w", piName, err)
				}
				// PackInstance already exists (basePackName supersession path).
				// Update spec to reflect the new ClusterPack version.
				existing := &seamv1alpha1.InfrastructurePackInstance{}
				if getErr := r.Client.Get(ctx, client.ObjectKey{Name: piName, Namespace: piNamespace}, existing); getErr != nil {
					return ctrl.Result{}, fmt.Errorf("get existing PackInstance %s: %w", piName, getErr)
				}
				specPatch := client.MergeFrom(existing.DeepCopy())
				existing.Spec.ClusterPackRef = pe.Spec.ClusterPackRef.Name
				existing.Spec.Version = cp.Spec.Version
				if patchErr := r.Client.Patch(ctx, existing, specPatch); patchErr != nil {
					return ctrl.Result{}, fmt.Errorf("patch PackInstance spec %s: %w", piName, patchErr)
				}
			}
			logger.Info("PackInstance created or updated",
				"name", piName, "namespace", piNamespace)

			// Write DeployedResources and UpgradeDirection to PackInstance status.
			// DeployedResources enables deletion cleanup (INV-006: no Jobs on delete path).
			// UpgradeDirection enables rollback tracking. wrapper-schema.md §3, Decision 11.
			piStatus := &seamv1alpha1.InfrastructurePackInstance{}
			piKey := client.ObjectKey{Name: piName, Namespace: piNamespace}
			if getErr := r.Client.Get(ctx, piKey, piStatus); getErr == nil {
				piPatch := client.MergeFrom(piStatus.DeepCopy())
				// Replace DeployedResources (not append) to reflect the current deployment.
				// On version supersession the previous resource list is replaced with
				// the new one so the deletion handler cleans up the correct resources.
				newRefs := make([]seamv1alpha1.InfrastructureDeployedResourceRef, 0, len(porDeployedResources))
				for _, dr := range porDeployedResources {
					newRefs = append(newRefs, seamv1alpha1.InfrastructureDeployedResourceRef(dr))
				}
				piStatus.Status.DeployedResources = newRefs
				piStatus.Status.UpgradeDirection = string(upgradeDir)
				if patchErr := r.Client.Status().Patch(ctx, piStatus, piPatch); patchErr != nil {
					logger.Error(patchErr, "failed to patch PackInstance status", "name", piName)
				}
			}

			return ctrl.Result{}, nil
		}

		if existingJob.Status.Failed > 0 {
			msg := fmt.Sprintf("pack-deploy Job %q failed. Check Job logs.", jobName)
			conditions.SetCondition(
				&pe.Status.Conditions,
				conditions.ConditionTypePackExecutionRunning,
				metav1.ConditionFalse,
				conditions.ReasonJobFailed,
				msg,
				pe.Generation,
			)
			conditions.SetCondition(
				&pe.Status.Conditions,
				conditions.ConditionTypePackExecutionFailed,
				metav1.ConditionTrue,
				conditions.ReasonJobFailed,
				msg,
				pe.Generation,
			)
			r.Recorder.Eventf(pe, nil, corev1.EventTypeWarning, "JobFailed", "JobFailed", msg)
			logger.Error(fmt.Errorf("job failed"), msg, "name", pe.Name, "jobName", jobName)
			return ctrl.Result{}, nil
		}

		// Job exists and is still running.
		conditions.SetCondition(
			&pe.Status.Conditions,
			conditions.ConditionTypePackExecutionRunning,
			metav1.ConditionTrue,
			conditions.ReasonJobSubmitted,
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
	conditions.SetCondition(
		&pe.Status.Conditions,
		conditions.ConditionTypePackExecutionRunning,
		metav1.ConditionTrue,
		conditions.ReasonJobSubmitted,
		fmt.Sprintf("pack-deploy Job %q submitted to Kueue.", jobName),
		pe.Generation,
	)
	r.Recorder.Eventf(pe, nil, corev1.EventTypeNormal, "JobSubmitted", "JobSubmitted",
		"pack-deploy Job %q submitted.", jobName)
	logger.Info("PackExecution pack-deploy Job submitted",
		"name", pe.Name, "jobName", jobName)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// isPermissionSnapshotCurrent reads the PermissionSnapshot for the target cluster
// via unstructured to avoid importing guardian types. Returns true if the snapshot
// has a Fresh=True condition. PermissionSnapshots live exclusively in seam-system
// and are named "snapshot-{clusterRef}". guardian-schema.md §PS condition vocabulary.
// Returns false (not error) if the snapshot is not found or not fresh.
func (r *PackExecutionReconciler) isPermissionSnapshotCurrent(ctx context.Context, pe *seamv1alpha1.InfrastructurePackExecution) (bool, error) {
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
// does not yet exist (Job will create it via rbac-intake) or has provisioned=true.
// Returns false only when the profile EXISTS but provisioned=false, blocking the Job
// until guardian reconciles it. Pack RBACProfiles live in seam-tenant-{targetCluster}.
// guardian-schema.md §RBACProfile.
func (r *PackExecutionReconciler) isRBACProfileProvisioned(ctx context.Context, pe *seamv1alpha1.InfrastructurePackExecution) (bool, error) {
	rp := &unstructured.Unstructured{}
	rp.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "security.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RBACProfile",
	})
	rpKey := types.NamespacedName{
		Name:      pe.Spec.AdmissionProfileRef,
		Namespace: "seam-tenant-" + pe.Spec.TargetClusterRef,
	}
	if err := r.Client.Get(ctx, rpKey, rp); err != nil {
		if apierrors.IsNotFound(err) {
			// Profile absent: Conductor Job creates it via rbac-intake. Allow Job to start.
			return true, nil
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
//  1. InfrastructureTalosCluster namespace (Fix 1): tries seam-tenant-{clusterRef}
//     first; if not found, falls back to seam-system. The management cluster
//     InfrastructureTalosCluster lives in seam-system, tenant cluster in
//     seam-tenant-{name}. If absent in both namespaces, cluster not yet registered.
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
func (r *PackExecutionReconciler) isConductorReadyForCluster(ctx context.Context, pe *seamv1alpha1.InfrastructurePackExecution) (bool, error) {
	clusterRef := pe.Spec.TargetClusterRef
	tcGVK := schema.GroupVersionKind{
		Group:   "infrastructure.ontai.dev",
		Version: "v1alpha1",
		Kind:    "InfrastructureTalosCluster",
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
		Group:   "infrastructure.ontai.dev",
		Version: "v1alpha1",
		Kind:    "InfrastructureRunnerConfig",
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

// isWrapperRunnerRBACReady performs SubjectAccessReview checks to verify that
// the wrapper-runner ServiceAccount in pe.Namespace has the required RBAC
// permissions before a Kueue Job is submitted. Returns (true, "", nil) when
// all permissions are granted. Returns (false, denyReason, nil) when any
// permission is denied. Returns (false, "", err) on SAR API failure.
//
// This is gate 5 of the PackExecution gate check. It catches stale or missing
// RBAC before the Job pod runs and fails with a Forbidden error. The three
// checks mirror the permissions declared in the compiler-generated
// wrapper-runner Role (05-post-bootstrap/wrapper-runner.yaml).
func (r *PackExecutionReconciler) isWrapperRunnerRBACReady(ctx context.Context, pe *seamv1alpha1.InfrastructurePackExecution) (bool, string, error) {
	saUser := "system:serviceaccount:" + pe.Namespace + ":wrapper-runner"

	checks := []struct {
		verb     string
		group    string
		resource string
	}{
		{"list", "infrastructure.ontai.dev", "infrastructurepackexecutions"},
		{"get", "infrastructure.ontai.dev", "infrastructurerunnerconfigs"},
		{"create", "infrastructure.ontai.dev", "packoperationresults"},
		{"delete", "infrastructure.ontai.dev", "packoperationresults"},
	}

	for _, chk := range checks {
		sar := &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User: saUser,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: pe.Namespace,
					Verb:      chk.verb,
					Group:     chk.group,
					Resource:  chk.resource,
				},
			},
		}
		if err := r.Client.Create(ctx, sar); err != nil {
			return false, "", fmt.Errorf("SubjectAccessReview %s %s.%s: %w", chk.verb, chk.resource, chk.group, err)
		}
		if !sar.Status.Allowed {
			reason := sar.Status.Reason
			if reason == "" {
				reason = fmt.Sprintf("wrapper-runner cannot %s %s in %s: permission denied", chk.verb, chk.resource, chk.group)
			}
			return false, reason, nil
		}
	}
	return true, "", nil
}

// buildPackDeployJob constructs the pack-deploy Job spec for Kueue admission.
// The Job carries the kueue.x-k8s.io/queue-name label — without this label,
// the Job will not be admitted. wrapper-design.md §4.
func (r *PackExecutionReconciler) buildPackDeployJob(
	pe *seamv1alpha1.InfrastructurePackExecution,
	cp *seamv1alpha1.InfrastructureClusterPack,
	jobName string,
) *batchv1.Job {
	ttl := packDeployJobTTL
	conductorImage := conductorImageDefault

	// resultCMName is the packExecutionRef passed to the Conductor Job via
	// OPERATION_RESULT_CM. Conductor uses this value as the label key for
	// the single-active-revision POR pattern (T-15). pe.Name is the PackExecution
	// CR name, which is the natural packExecutionRef.
	resultCMName := pe.Name

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: pe.Namespace,
			Labels: map[string]string{
				kueueQueueLabel:              packDeployQueue,
				"app.kubernetes.io/part-of": "wrapper",
				"infrastructure.ontai.dev/pe-name":   pe.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         seamv1alpha1.GroupVersion.String(),
					Kind:               "InfrastructurePackExecution",
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
									SecretName: platformKubeconfigSecretName(pe.Spec.TargetClusterRef),
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
func packDeployJobName(pe *seamv1alpha1.InfrastructurePackExecution) string {
	return fmt.Sprintf("pack-deploy-%s", pe.Name)
}

// platformKubeconfigSecretName returns the name of the kubeconfig Secret created by
// Platform for the given cluster. Platform writes seam-mc-{clusterRef}-kubeconfig
// to seam-tenant-{clusterRef} for all cluster roles. Wrapper Jobs mount this Secret
// directly rather than maintaining a separate copy. platform-schema.md §5.
func platformKubeconfigSecretName(clusterRef string) string {
	return "seam-mc-" + clusterRef + "-kubeconfig"
}

func boolPtr(b bool) *bool { return &b }

func int32Ptr(i int32) *int32 { return &i }

// SetupWithManager registers PackExecutionReconciler as the controller for PackExecution.
// WS3: Watches PermissionSnapshot and RBACProfile so the reconciler is triggered
// immediately when gates clear, instead of waiting for the 30s gateRequeueInterval.
// CONDUCTOR-BL-CAPABILITY-WATCH: Watches RunnerConfig so the ConductorReady gate
// (gate 0) re-evaluates immediately when capabilities are published to RunnerConfig
// status, instead of waiting for the 30s gateRequeueInterval poll.
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
	rcObj := &unstructured.Unstructured{}
	rcObj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "runner.ontai.dev",
		Version: "v1alpha1",
		Kind:    "RunnerConfig",
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&seamv1alpha1.InfrastructurePackExecution{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.Job{}).
		Watches(psObj, handler.EnqueueRequestsFromMapFunc(r.mapSnapshotToPackExecutions)).
		Watches(rpObj, handler.EnqueueRequestsFromMapFunc(r.mapRBACProfileToPackExecutions)).
		Watches(rcObj, handler.EnqueueRequestsFromMapFunc(r.mapRunnerConfigToPackExecutions)).
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
	peList := &seamv1alpha1.InfrastructurePackExecutionList{}
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

// packUpgradeDirection determines the upgrade direction for a PackInstance by
// comparing the existing version (if any) with newVersion. Returns:
//
//	Initial  — no existing PackInstance found
//	Redeploy — same version reapplied
//	Upgrade  — newVersion is newer than existing
//	Rollback — newVersion is older than existing
func packUpgradeDirection(
	ctx context.Context,
	c client.Client,
	piName, piNamespace, newVersion string,
) seamv1alpha1.PackUpgradeDirection {
	existing := &seamv1alpha1.InfrastructurePackInstance{}
	if err := c.Get(ctx, client.ObjectKey{Name: piName, Namespace: piNamespace}, existing); err != nil {
		return seamv1alpha1.PackUpgradeDirectionInitial
	}
	existingVer := existing.Spec.Version
	if existingVer == "" {
		return seamv1alpha1.PackUpgradeDirectionInitial
	}
	cmp := comparePackVersion(existingVer, newVersion)
	switch {
	case cmp == 0:
		return seamv1alpha1.PackUpgradeDirectionRedeploy
	case cmp < 0:
		return seamv1alpha1.PackUpgradeDirectionUpgrade
	default:
		return seamv1alpha1.PackUpgradeDirectionRollback
	}
}

// comparePackVersion compares two pack versions in vX.Y.Z-rN format.
// Returns negative if a < b, zero if equal, positive if a > b.
// Falls back to lexicographic comparison if format is unrecognised.
func comparePackVersion(a, b string) int {
	av := parsePackVer(a)
	bv := parsePackVer(b)
	for i := range av {
		if av[i] != bv[i] {
			return av[i] - bv[i]
		}
	}
	return 0
}

// parsePackVer parses a pack version string into a [4]int of {major, minor, patch, rev}.
// Unrecognised formats return all zeros.
func parsePackVer(v string) [4]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, "-r", 2)
	semParts := strings.SplitN(parts[0], ".", 3)
	var out [4]int
	if len(semParts) == 3 {
		out[0], _ = strconv.Atoi(semParts[0])
		out[1], _ = strconv.Atoi(semParts[1])
		out[2], _ = strconv.Atoi(semParts[2])
	}
	if len(parts) == 2 {
		out[3], _ = strconv.Atoi(parts[1])
	}
	return out
}

// mapRunnerConfigToPackExecutions maps a RunnerConfig update to PackExecution requests
// in the seam-tenant-{cluster} namespace for the cluster that owns the RunnerConfig.
// RunnerConfig lives in ont-system and is named after the cluster (e.g. "ccs-dev").
// When capabilities are first published to RunnerConfig status, the ConductorReady
// gate (gate 0) clears and pending PackExecutions for that cluster can proceed.
// CONDUCTOR-BL-CAPABILITY-WATCH.
func (r *PackExecutionReconciler) mapRunnerConfigToPackExecutions(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	// RunnerConfig name == cluster name; lives in ont-system.
	if obj.GetNamespace() != "ont-system" {
		return nil
	}
	clusterRef := obj.GetName()
	ns := "seam-tenant-" + clusterRef
	peList := &seamv1alpha1.InfrastructurePackExecutionList{}
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
	peList := &seamv1alpha1.InfrastructurePackExecutionList{}
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

// findLatestPOR lists all PackOperationResult CRs in namespace that carry the
// label ontai.dev/pack-execution={packExecutionRef} and returns the one with the
// highest Revision. Returns nil (not an error) when no POR exists yet.
// Implements the single-active-revision read path (T-16). seam-core-schema.md §8.
func findLatestPOR(
	ctx context.Context,
	c client.Client,
	namespace, packExecutionRef string,
) (*seamv1alpha1.PackOperationResult, error) {
	list := &seamv1alpha1.PackOperationResultList{}
	if err := c.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{labelPackExecution: packExecutionRef},
	); err != nil {
		return nil, fmt.Errorf("findLatestPOR: list %q in %q: %w", packExecutionRef, namespace, err)
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	best := &list.Items[0]
	for i := 1; i < len(list.Items); i++ {
		if list.Items[i].Spec.Revision > best.Spec.Revision {
			best = &list.Items[i]
		}
	}
	return best, nil
}
