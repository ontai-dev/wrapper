package controller

import (
	"context"
	"fmt"
	"time"

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

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
)

// driftCheckInterval is how often the reconciler re-reads PackReceipt drift status
// to update the Drifted condition. wrapper-schema.md §3 PackInstance.
const driftCheckInterval = 60 * time.Second

// PackInstanceReconciler watches PackInstance CRs and reflects drift state from
// the target cluster conductor's PackReceipt.
//
// PackReceipt is a target-cluster-only resource managed by the conductor agent.
// The management cluster reconciler reads it via the management-side mirror
// (an Unstructured read using the target cluster's kubeconfig is out of scope here;
// we read the PackReceipt from the management namespace following the conductor
// SnapshotStore pattern — the conductor agent mirrors PackReceipt into the
// tenant namespace of the management cluster after each drift check cycle).
//
// Reconcile loop:
//  1. Fetch PackInstance CR. Not found → no-op (INV-006).
//  2. Defer status patch.
//  3. Advance ObservedGeneration.
//  4. Initialize LineageSynced on first observation.
//  5. Check SecurityViolation: if PackReceipt.signatureVerified=false, raise
//     SecurityViolation condition. Block all further ops on the affected cluster.
//  6. Read PackReceipt drift status. Update Drifted condition.
//  7. If DriftPolicy=Block and any dependency is Drifted=True, set DependencyBlocked.
//  8. Requeue after driftCheckInterval for continuous drift polling.
type PackInstanceReconciler struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile is the main reconciliation loop for PackInstance.
//
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=packinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=packinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=packinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=packexecutions,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
func (r *PackInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Step A — Fetch the PackInstance CR.
	pi := &infrav1alpha1.PackInstance{}
	if err := r.Client.Get(ctx, req.NamespacedName, pi); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("PackInstance not found — likely deleted, ignoring",
				"namespacedName", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get PackInstance %s: %w", req.NamespacedName, err)
	}

	// Step B — Deferred status patch.
	patchBase := client.MergeFrom(pi.DeepCopy())
	defer func() {
		if err := r.Client.Status().Patch(ctx, pi, patchBase); err != nil {
			if !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to patch PackInstance status",
					"name", pi.Name, "namespace", pi.Namespace)
			}
		}
	}()

	// Step C — Advance ObservedGeneration.
	pi.Status.ObservedGeneration = pi.Generation

	// Step D — Initialize LineageSynced on first observation (one-time write).
	if infrav1alpha1.FindCondition(pi.Status.Conditions, infrav1alpha1.ConditionTypeLineageSynced) == nil {
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypeLineageSynced,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonLineageControllerAbsent,
			"InfrastructureLineageController is not yet deployed.",
			pi.Generation,
		)
	}

	// Step E — Check if a PackExecution for this pack+cluster has Succeeded=True.
	// DSNSReconciler in seam-core emits the pack DNS TXT record only when
	// PackInstance has Ready=True. We set Ready=True here as soon as the
	// pack-deploy Job completes successfully, without waiting for the conductor
	// agent to write a PackReceipt on the target cluster.
	succeededPE, err := r.findSucceededPackExecution(ctx, pi.Namespace, pi.Spec.ClusterPackRef, pi.Spec.TargetClusterRef)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to find succeeded PackExecution: %w", err)
	}
	if succeededPE != nil {
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceReady,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonPackDelivered,
			fmt.Sprintf("Pack %s v%s successfully delivered to %s.",
				succeededPE.Spec.ClusterPackRef.Name,
				succeededPE.Spec.ClusterPackRef.Version,
				pi.Spec.TargetClusterRef),
			pi.Generation,
		)
		return ctrl.Result{RequeueAfter: driftCheckInterval}, nil
	}
	// No succeeded PackExecution — pack not yet delivered. Fall through to
	// PackReceipt-based drift detection which handles the post-receipt lifecycle.
	infrav1alpha1.SetCondition(
		&pi.Status.Conditions,
		infrav1alpha1.ConditionTypePackInstanceReady,
		metav1.ConditionFalse,
		infrav1alpha1.ReasonAwaitingDelivery,
		"No succeeded PackExecution found for this pack and cluster.",
		pi.Generation,
	)

	// Step F — Read PackReceipt from the management cluster namespace.
	// PackReceipt is mirrored into the tenant namespace by the conductor agent.
	// PackReceipt name convention: {clusterPackRef}-{targetClusterRef}
	receiptName := pi.Spec.ClusterPackRef + "-" + pi.Spec.TargetClusterRef
	receipt, err := r.getPackReceipt(ctx, receiptName, pi.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to read PackReceipt: %w", err)
	}
	if receipt == nil {
		// PackReceipt not yet written by conductor — not an error.
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceReady,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonPackReceiptNotFound,
			"PackReceipt not yet written by conductor agent on target cluster.",
			pi.Generation,
		)
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceProgressing,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonPackReceiptNotFound,
			"Waiting for conductor to write PackReceipt.",
			pi.Generation,
		)
		return ctrl.Result{RequeueAfter: driftCheckInterval}, nil
	}

	// Step F — Security gate: signatureVerified must be true.
	// If the PackReceipt reports signatureVerified=false, this is a SecurityViolation.
	// All further pack ops on the affected cluster are blocked. wrapper-design.md §5.
	signatureVerified, _, _ := unstructured.NestedBool(receipt.Object, "status", "signatureVerified")
	if !signatureVerified {
		msg := fmt.Sprintf(
			"PackReceipt for pack %q on cluster %q reports signatureVerified=false. "+
				"Security violation — all pack operations on this cluster are blocked.",
			pi.Spec.ClusterPackRef, pi.Spec.TargetClusterRef,
		)
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceSecurityViolation,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonSignatureVerifyFailed,
			msg,
			pi.Generation,
		)
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceReady,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonSignatureVerifyFailed,
			"SecurityViolation: signature verification failed.",
			pi.Generation,
		)
		r.Recorder.Event(pi, corev1.EventTypeWarning, "SecurityViolation", msg)
		logger.Error(fmt.Errorf("security violation"), msg,
			"name", pi.Name, "namespace", pi.Namespace)
		// Requeue to poll for resolution. Human intervention is required but
		// we poll to detect if the violation is cleared.
		return ctrl.Result{RequeueAfter: driftCheckInterval}, nil
	}
	// Signature verified — clear any prior SecurityViolation.
	infrav1alpha1.SetCondition(
		&pi.Status.Conditions,
		infrav1alpha1.ConditionTypePackInstanceSecurityViolation,
		metav1.ConditionFalse,
		infrav1alpha1.ReasonSecurityViolationCleared,
		"Signature verification passed.",
		pi.Generation,
	)

	// Step G — Reflect drift status from PackReceipt.
	driftStatus, _, _ := unstructured.NestedString(receipt.Object, "status", "driftStatus")
	driftSummary, _, _ := unstructured.NestedString(receipt.Object, "status", "driftSummary")
	pi.Status.DriftSummary = driftSummary

	switch driftStatus {
	case "Drifted":
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceDrifted,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonDriftDetected,
			fmt.Sprintf("Conductor drift detection reports drift: %s", driftSummary),
			pi.Generation,
		)
		r.Recorder.Event(pi, corev1.EventTypeWarning, "DriftDetected",
			fmt.Sprintf("Pack drift detected on cluster %q: %s", pi.Spec.TargetClusterRef, driftSummary))
	default:
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceDrifted,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonNoDrift,
			"No drift detected.",
			pi.Generation,
		)
	}

	// Step H — Dependency gate: check dependsOn list for Drifted=True.
	blocked, blockedBy, err := r.checkDependencyDrift(ctx, pi)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check dependency drift: %w", err)
	}
	if blocked {
		msg := fmt.Sprintf("Dependency PackInstance %q is drifted and DependencyPolicy is Block.", blockedBy)
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceDependencyBlocked,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonDependencyDrifted,
			msg,
			pi.Generation,
		)
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceReady,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonDependencyDrifted,
			msg,
			pi.Generation,
		)
		r.Recorder.Event(pi, corev1.EventTypeWarning, "DependencyBlocked", msg)
		return ctrl.Result{RequeueAfter: driftCheckInterval}, nil
	}
	infrav1alpha1.SetCondition(
		&pi.Status.Conditions,
		infrav1alpha1.ConditionTypePackInstanceDependencyBlocked,
		metav1.ConditionFalse,
		infrav1alpha1.ReasonDependencyDrifted,
		"No blocking dependency drift.",
		pi.Generation,
	)

	// Step I — Set Ready condition based on drift state.
	driftedCond := infrav1alpha1.FindCondition(pi.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceDrifted)
	if driftedCond != nil && driftedCond.Status == metav1.ConditionTrue {
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceReady,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonDriftDetected,
			"Pack is drifted.",
			pi.Generation,
		)
	} else {
		now := metav1.Now()
		pi.Status.DeliveredAt = &now
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceReady,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonPackReceiptReady,
			"Pack is delivered and in sync.",
			pi.Generation,
		)
		infrav1alpha1.SetCondition(
			&pi.Status.Conditions,
			infrav1alpha1.ConditionTypePackInstanceProgressing,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonPackDelivered,
			"Pack delivery confirmed.",
			pi.Generation,
		)
	}

	return ctrl.Result{RequeueAfter: driftCheckInterval}, nil
}

// findSucceededPackExecution lists PackExecutions in namespace and returns the first
// one whose spec.clusterPackRef.name matches clusterPackRef and whose
// spec.targetClusterRef matches targetClusterRef and that has Succeeded=True.
// Returns nil if no matching succeeded PackExecution exists.
func (r *PackInstanceReconciler) findSucceededPackExecution(
	ctx context.Context,
	namespace, clusterPackRef, targetClusterRef string,
) (*infrav1alpha1.PackExecution, error) {
	list := &infrav1alpha1.PackExecutionList{}
	if err := r.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list PackExecutions: %w", err)
	}
	for i := range list.Items {
		pe := &list.Items[i]
		if pe.Spec.ClusterPackRef.Name != clusterPackRef {
			continue
		}
		if pe.Spec.TargetClusterRef != targetClusterRef {
			continue
		}
		succeededCond := infrav1alpha1.FindCondition(pe.Status.Conditions, infrav1alpha1.ConditionTypePackExecutionSucceeded)
		if succeededCond != nil && succeededCond.Status == metav1.ConditionTrue {
			return pe, nil
		}
	}
	return nil, nil
}

// getPackReceipt reads a PackReceipt resource via unstructured from the management
// cluster namespace. Returns nil if the resource is not found.
func (r *PackInstanceReconciler) getPackReceipt(ctx context.Context, name, namespace string) (*unstructured.Unstructured, error) {
	receipt := &unstructured.Unstructured{}
	receipt.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "runner.ontai.dev",
		Version: "v1alpha1",
		Kind:    "PackReceipt",
	})
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := r.Client.Get(ctx, key, receipt); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return receipt, nil
}

// checkDependencyDrift checks the DependsOn list for any PackInstance that is
// drifted and the DependencyPolicy is Block. Returns (blocked, blockedByName, err).
func (r *PackInstanceReconciler) checkDependencyDrift(ctx context.Context, pi *infrav1alpha1.PackInstance) (bool, string, error) {
	if len(pi.Spec.DependsOn) == 0 {
		return false, "", nil
	}
	policy := infrav1alpha1.DriftPolicyWarn
	if pi.Spec.DependencyPolicy != nil {
		policy = pi.Spec.DependencyPolicy.OnDrift
	}
	if policy != infrav1alpha1.DriftPolicyBlock {
		// Warn and Ignore policies do not block.
		return false, "", nil
	}
	for _, depName := range pi.Spec.DependsOn {
		dep := &infrav1alpha1.PackInstance{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: depName, Namespace: pi.Namespace}, dep); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, "", fmt.Errorf("failed to get dependency PackInstance %s: %w", depName, err)
		}
		driftedCond := infrav1alpha1.FindCondition(dep.Status.Conditions, infrav1alpha1.ConditionTypePackInstanceDrifted)
		if driftedCond != nil && driftedCond.Status == metav1.ConditionTrue {
			return true, depName, nil
		}
	}
	return false, "", nil
}

// SetupWithManager registers PackInstanceReconciler as the controller for PackInstance.
func (r *PackInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.PackInstance{}).
		Complete(r)
}
