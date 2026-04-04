package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
)

// packSignatureAnnotation is the annotation key set by the conductor signing loop
// on a ClusterPack CR after Ed25519 signing. The reconciler reads this annotation
// to transition the ClusterPack from SignaturePending to Available.
// INV-026: signing is management cluster conductor's responsibility only.
const packSignatureAnnotation = "ontai.dev/pack-signature"

// ClusterPackReconciler watches ClusterPack CRs and manages their signing lifecycle
// and immutability enforcement.
//
// Reconcile loop:
//  1. Fetch ClusterPack. Not found → no-op (INV-006).
//  2. Defer status patch.
//  3. Advance ObservedGeneration.
//  4. Initialize LineageSynced on first observation.
//  5. Enforce spec immutability by comparing against the stored snapshot annotation.
//  6. If pack is revoked (Revoked=True), emit event and stop. No requeue.
//  7. If signature annotation is present and status.Signed=false → set Signed=true,
//     copy signature to status, set Available=True, clear SignaturePending.
//  8. If not yet signed → set SignaturePending=True. Requeue after 15s.
type ClusterPackReconciler struct {
	Client   client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile is the main reconciliation loop for ClusterPack.
//
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=clusterpacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=clusterpacks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=clusterpacks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
func (r *ClusterPackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Step A — Fetch the ClusterPack CR.
	cp := &infrav1alpha1.ClusterPack{}
	if err := r.Client.Get(ctx, req.NamespacedName, cp); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("ClusterPack not found — likely deleted, ignoring",
				"namespacedName", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ClusterPack %s: %w", req.NamespacedName, err)
	}

	// Step B — Record spec snapshot annotation on first reconcile, BEFORE setting
	// up the deferred status patch. This must happen first because calling
	// r.Client.Patch() after the status patch setup would overwrite the in-memory
	// object with the stored state, losing any status mutations made before the call.
	// CI-INV-002: ClusterPack spec is immutable after creation.
	const specSnapshotAnnotation = "infra.ontai.dev/spec-checksum-snapshot"
	currentChecksum := cp.Spec.Checksum + "|" + cp.Spec.RegistryRef.URL + "|" + cp.Spec.RegistryRef.Digest + "|" + cp.Spec.Version
	if _, ok := cp.Annotations[specSnapshotAnnotation]; !ok {
		metaPatch := client.MergeFrom(cp.DeepCopy())
		if cp.Annotations == nil {
			cp.Annotations = map[string]string{}
		}
		cp.Annotations[specSnapshotAnnotation] = currentChecksum
		if err := r.Client.Patch(ctx, cp, metaPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to record spec snapshot annotation: %w", err)
		}
	}

	// Step C — Deferred status patch. patchBase is taken AFTER the metadata patch
	// to ensure the deferred status patch operates on the current ResourceVersion.
	patchBase := client.MergeFrom(cp.DeepCopy())
	defer func() {
		if err := r.Client.Status().Patch(ctx, cp, patchBase); err != nil {
			if !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to patch ClusterPack status",
					"name", cp.Name, "namespace", cp.Namespace)
			}
		}
	}()

	// Step D — Advance ObservedGeneration.
	cp.Status.ObservedGeneration = cp.Generation

	// Step E — Initialize LineageSynced on first observation (one-time write).
	// seam-core-schema.md §7 Declaration 5.
	if infrav1alpha1.FindCondition(cp.Status.Conditions, infrav1alpha1.ConditionTypeLineageSynced) == nil {
		infrav1alpha1.SetCondition(
			&cp.Status.Conditions,
			infrav1alpha1.ConditionTypeLineageSynced,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonLineageControllerAbsent,
			"InfrastructureLineageController is not yet deployed.",
			cp.Generation,
		)
	}

	// Step F — Enforce spec immutability. The snapshot annotation was recorded in
	// step B. Any divergence between the current spec and the snapshot is a security event.
	if stored := cp.Annotations[specSnapshotAnnotation]; stored != currentChecksum {
		msg := "ClusterPack spec mutation detected — spec is immutable after creation. CI-INV-002."
		infrav1alpha1.SetCondition(
			&cp.Status.Conditions,
			infrav1alpha1.ConditionTypeClusterPackImmutabilityViolation,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonImmutabilityViolation,
			msg,
			cp.Generation,
		)
		r.Recorder.Event(cp, corev1.EventTypeWarning, "ImmutabilityViolation", msg)
		logger.Error(fmt.Errorf("immutability violation"), msg,
			"name", cp.Name, "namespace", cp.Namespace)
		return ctrl.Result{}, nil
	}

	// Step F — Check revocation. If Revoked=True, stop. Human intervention required.
	revokedCond := infrav1alpha1.FindCondition(cp.Status.Conditions, infrav1alpha1.ConditionTypeClusterPackRevoked)
	if revokedCond != nil && revokedCond.Status == metav1.ConditionTrue {
		logger.Info("ClusterPack is revoked — no further reconciliation",
			"name", cp.Name, "namespace", cp.Namespace)
		return ctrl.Result{}, nil
	}

	// Step G — Check for conductor signature annotation (INV-026).
	// When present and status not yet updated, transition to Available.
	sig, hasSig := cp.Annotations[packSignatureAnnotation]
	if hasSig && !cp.Status.Signed {
		cp.Status.Signed = true
		cp.Status.PackSignature = sig
		infrav1alpha1.SetCondition(
			&cp.Status.Conditions,
			infrav1alpha1.ConditionTypeClusterPackSignaturePending,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonPackSigned,
			"Pack has been signed by the conductor signing loop.",
			cp.Generation,
		)
		infrav1alpha1.SetCondition(
			&cp.Status.Conditions,
			infrav1alpha1.ConditionTypeClusterPackAvailable,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonPackAvailable,
			"Pack is signed and available for deployment.",
			cp.Generation,
		)
		r.Recorder.Event(cp, corev1.EventTypeNormal, "PackSigned", "ClusterPack signed and now available.")
		logger.Info("ClusterPack transitioned to Available",
			"name", cp.Name, "namespace", cp.Namespace)
		return ctrl.Result{}, nil
	}

	// Step H — Not yet signed. Ensure SignaturePending condition and requeue.
	if !cp.Status.Signed {
		infrav1alpha1.SetCondition(
			&cp.Status.Conditions,
			infrav1alpha1.ConditionTypeClusterPackSignaturePending,
			metav1.ConditionTrue,
			infrav1alpha1.ReasonPackSignaturePending,
			"Waiting for conductor signing loop to sign this pack.",
			cp.Generation,
		)
		infrav1alpha1.SetCondition(
			&cp.Status.Conditions,
			infrav1alpha1.ConditionTypeClusterPackAvailable,
			metav1.ConditionFalse,
			infrav1alpha1.ReasonPackSignaturePending,
			"Pack not yet signed.",
			cp.Generation,
		)
		logger.Info("ClusterPack awaiting conductor signature — requeueing",
			"name", cp.Name, "namespace", cp.Namespace)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers ClusterPackReconciler as the controller for ClusterPack.
func (r *ClusterPackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.ClusterPack{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
