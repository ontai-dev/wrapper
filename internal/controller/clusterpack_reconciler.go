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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seamcorev1alpha1 "github.com/ontai-dev/seam-core/api/v1alpha1"
	"github.com/ontai-dev/seam-core/pkg/conditions"
)

// packSignatureAnnotation is the annotation key set by the conductor signing loop
// on a ClusterPack CR after Ed25519 signing. The reconciler reads this annotation
// to transition the ClusterPack from SignaturePending to Available.
// INV-026: signing is management cluster conductor's responsibility only.
const packSignatureAnnotation = "ontai.dev/pack-signature"

// clusterPackFinalizer is added to every ClusterPack on first reconcile.
// On deletion it triggers cleanup of all PackInstances and RunnerConfigs
// derived from this ClusterPack before the object is removed from etcd.
const clusterPackFinalizer = "infrastructure.ontai.dev/clusterpack-cleanup"

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
	Recorder clientevents.EventRecorder
}

// Reconcile is the main reconciliation loop for ClusterPack.
//
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructureclusterpacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructureclusterpacks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructureclusterpacks/finalizers,verbs=update
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructurepackinstances,verbs=get;list;delete
// +kubebuilder:rbac:groups=infrastructure.ontai.dev,resources=infrastructurepackexecutions,verbs=get;list;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
func (r *ClusterPackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Step A — Fetch the ClusterPack CR.
	cp := &seamcorev1alpha1.InfrastructureClusterPack{}
	if err := r.Client.Get(ctx, req.NamespacedName, cp); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("ClusterPack not found — likely deleted, ignoring",
				"namespacedName", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ClusterPack %s: %w", req.NamespacedName, err)
	}

	// Step A1 — Finalizer management.
	// If the ClusterPack is being deleted, run cleanup and return.
	// Otherwise, ensure the finalizer is present before any further reconciliation.
	if !cp.DeletionTimestamp.IsZero() {
		return r.handleClusterPackDeletion(ctx, cp)
	}
	if !containsString(cp.Finalizers, clusterPackFinalizer) {
		cp.Finalizers = append(cp.Finalizers, clusterPackFinalizer)
		if err := r.Client.Update(ctx, cp); err != nil {
			return ctrl.Result{}, fmt.Errorf("add ClusterPack finalizer: %w", err)
		}
		// Continue in this pass: the finalizer update does not change generation
		// or annotations, so the resulting watch event is filtered out by the
		// GenerationChangedPredicate|AnnotationChangedPredicate on SetupWithManager.
		// Falling through ensures status conditions are set in this reconcile pass.
	}

	// Step A2 — Rollback check. Evaluated before the spec-snapshot annotation so that
	// a Governor-authorized rollback (spec.rollbackToRevision > 0) can patch the spec
	// and clear the annotation without triggering the immutability gate. On the next
	// reconcile pass the annotation is absent, gets re-recorded from the rolled-back
	// spec, and normal reconciliation proceeds. wrapper-schema.md §6.2.
	if cp.Spec.RollbackToRevision > 0 {
		return r.handleRollback(ctx, cp)
	}

	// Step B — Record spec snapshot annotation on first reconcile, BEFORE setting
	// up the deferred status patch. This must happen first because calling
	// r.Client.Patch() after the status patch setup would overwrite the in-memory
	// object with the stored state, losing any status mutations made before the call.
	// CI-INV-002: ClusterPack spec is immutable after creation.
	const specSnapshotAnnotation = "infrastructure.ontai.dev/spec-checksum-snapshot"
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
	if conditions.FindCondition(cp.Status.Conditions, conditions.ConditionTypeLineageSynced) == nil {
		conditions.SetCondition(
			&cp.Status.Conditions,
			conditions.ConditionTypeLineageSynced,
			metav1.ConditionFalse,
			conditions.ReasonLineageControllerAbsent,
			"InfrastructureLineageController is not yet deployed.",
			cp.Generation,
		)
	}

	// Step F — Enforce spec immutability. The snapshot annotation was recorded in
	// step B. Any divergence between the current spec and the snapshot is a security event.
	if stored := cp.Annotations[specSnapshotAnnotation]; stored != currentChecksum {
		msg := "ClusterPack spec mutation detected — spec is immutable after creation. CI-INV-002."
		conditions.SetCondition(
			&cp.Status.Conditions,
			conditions.ConditionTypeClusterPackImmutabilityViolation,
			metav1.ConditionTrue,
			conditions.ReasonImmutabilityViolation,
			msg,
			cp.Generation,
		)
		r.Recorder.Eventf(cp, nil, corev1.EventTypeWarning, "ImmutabilityViolation", "ImmutabilityViolation", msg)
		logger.Error(fmt.Errorf("immutability violation"), msg,
			"name", cp.Name, "namespace", cp.Namespace)
		return ctrl.Result{}, nil
	}

	// Step F — Check revocation. If Revoked=True, stop. Human intervention required.
	revokedCond := conditions.FindCondition(cp.Status.Conditions, conditions.ConditionTypeClusterPackRevoked)
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
		conditions.SetCondition(
			&cp.Status.Conditions,
			conditions.ConditionTypeClusterPackSignaturePending,
			metav1.ConditionFalse,
			conditions.ReasonPackSigned,
			"Pack has been signed by the conductor signing loop.",
			cp.Generation,
		)
		conditions.SetCondition(
			&cp.Status.Conditions,
			conditions.ConditionTypeClusterPackAvailable,
			metav1.ConditionTrue,
			conditions.ReasonPackAvailable,
			"Pack is signed and available for deployment.",
			cp.Generation,
		)
		r.Recorder.Eventf(cp, nil, corev1.EventTypeNormal, "PackSigned", "PackSigned", "ClusterPack signed and now available.")
		logger.Info("ClusterPack transitioned to Available",
			"name", cp.Name, "namespace", cp.Namespace)
		// Fall through to Step I — provision RunnerConfigs in the same reconcile pass.
	}

	// Step H — Not yet signed. Ensure SignaturePending condition and requeue.
	if !cp.Status.Signed {
		conditions.SetCondition(
			&cp.Status.Conditions,
			conditions.ConditionTypeClusterPackSignaturePending,
			metav1.ConditionTrue,
			conditions.ReasonPackSignaturePending,
			"Waiting for conductor signing loop to sign this pack.",
			cp.Generation,
		)
		conditions.SetCondition(
			&cp.Status.Conditions,
			conditions.ConditionTypeClusterPackAvailable,
			metav1.ConditionFalse,
			conditions.ReasonPackSignaturePending,
			"Pack not yet signed.",
			cp.Generation,
		)
		logger.Info("ClusterPack awaiting conductor signature — requeueing",
			"name", cp.Name, "namespace", cp.Namespace)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Step I — Direct PackExecution provisioning: create one PackExecution per
	// targetCluster in seam-tenant-{clusterName}. Idempotent: skips if PackInstance
	// with current version already exists (delivery complete) or PackExecution already
	// exists (delivery in progress). wrapper-schema.md §9 delivery chain.
	for _, clusterName := range cp.Spec.TargetClusters {
		tenantNS := "seam-tenant-" + clusterName
		peName := cp.Name + "-" + clusterName

		// If PackInstance exists with current version, delivery is already complete — skip.
		existingPI := &seamcorev1alpha1.InfrastructurePackInstance{}
		piErr := r.Client.Get(ctx, client.ObjectKey{Name: peName, Namespace: tenantNS}, existingPI)
		if piErr == nil {
			if existingPI.Spec.Version == cp.Spec.Version {
				logger.Info("PackInstance exists with current version — skipping PackExecution creation",
					"pack", cp.Name, "cluster", clusterName, "version", cp.Spec.Version)
				continue
			}
			// Version mismatch: the PackExecution reconciler will delete the stale
			// PackExecution (WS4 in packexecution_reconciler.go). Fall through so
			// the AlreadyExists guard below handles the case where a fresh one exists.
			logger.Info("PackInstance version mismatch — PackExecution reconciler will handle stale object",
				"pack", cp.Name, "cluster", clusterName,
				"currentVersion", existingPI.Spec.Version, "targetVersion", cp.Spec.Version)
		} else if !apierrors.IsNotFound(piErr) {
			return ctrl.Result{}, fmt.Errorf("get PackInstance %s/%s: %w", tenantNS, peName, piErr)
		}

		// Skip if PackExecution already exists — delivery is in progress.
		existingPE := &seamcorev1alpha1.InfrastructurePackExecution{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: peName, Namespace: tenantNS}, existingPE); err == nil {
			logger.Info("PackExecution exists — skipping creation",
				"pack", cp.Name, "cluster", clusterName)
			continue
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get PackExecution %s/%s: %w", tenantNS, peName, err)
		}

		// Create PackExecution directly for pack delivery to this cluster.
		newPE := &seamcorev1alpha1.InfrastructurePackExecution{
			ObjectMeta: metav1.ObjectMeta{
				Name:      peName,
				Namespace: tenantNS,
				Labels: map[string]string{
					"infrastructure.ontai.dev/pack":         cp.Name,
					"infrastructure.ontai.dev/pack-version": cp.Spec.Version,
					"infrastructure.ontai.dev/cluster":      clusterName,
				},
			},
			Spec: seamcorev1alpha1.InfrastructurePackExecutionSpec{
				ClusterPackRef: seamcorev1alpha1.InfrastructureClusterPackRef{
					Name:    cp.Name,
					Version: cp.Spec.Version,
				},
				TargetClusterRef:    clusterName,
				AdmissionProfileRef: cp.Name,
			},
		}
		if err := r.Client.Create(ctx, newPE); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create PackExecution %s/%s: %w", tenantNS, peName, err)
		}
		logger.Info("PackExecution created for pack delivery",
			"pack", cp.Name, "version", cp.Spec.Version, "cluster", clusterName, "packExecution", peName)
		r.Recorder.Eventf(cp, nil, corev1.EventTypeNormal, "PackExecutionCreated", "PackExecutionCreated",
			"PackExecution %s created in %s for pack delivery to cluster %s.", peName, tenantNS, clusterName)
	}

	return ctrl.Result{}, nil
}

// handleRollback processes a Governor-initiated rollback request (spec.rollbackToRevision > 0).
// It lists ALL PORs for this ClusterPack (both active and superseded), finds the one at
// spec.rollbackToRevision, reads its version/digest fields, patches the ClusterPack spec
// back to that artifact state, clears the spec-snapshot annotation (so the immutability check
// re-records the rolled-back state on the next reconcile), and clears rollbackToRevision.
// N-step rollback: any prior retained revision is reachable in one operation.
// wrapper-schema.md §6.2. seam-core-schema.md §7.8.
func (r *ClusterPackReconciler) handleRollback(ctx context.Context, cp *seamcorev1alpha1.InfrastructureClusterPack) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	targetRevision := cp.Spec.RollbackToRevision

	// List ALL PORs for this ClusterPack: active and superseded.
	porList := &seamcorev1alpha1.PackOperationResultList{}
	if err := r.Client.List(ctx, porList,
		client.InNamespace(cp.Namespace),
		client.MatchingLabels{"ontai.dev/cluster-pack": cp.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("handleRollback: list PORs for %s: %w", cp.Name, err)
	}

	if len(porList.Items) == 0 {
		logger.Info("handleRollback: no POR found for ClusterPack — cannot rollback, clearing field",
			"clusterPack", cp.Name, "namespace", cp.Namespace)
		return r.clearRollbackField(ctx, cp)
	}

	// Find the POR at exactly the requested revision.
	var targetPOR *seamcorev1alpha1.PackOperationResult
	for i := range porList.Items {
		if porList.Items[i].Spec.Revision == targetRevision {
			targetPOR = &porList.Items[i]
			break
		}
	}

	if targetPOR == nil {
		logger.Info("handleRollback: target revision not found in retained POR history — clearing field",
			"clusterPack", cp.Name, "requestedRevision", targetRevision)
		return r.clearRollbackField(ctx, cp)
	}

	targetVersion := targetPOR.Spec.ClusterPackVersion
	targetRBACDigest := targetPOR.Spec.RBACDigest
	targetWorkloadDigest := targetPOR.Spec.WorkloadDigest

	if targetVersion == "" {
		logger.Info("handleRollback: target POR has no version recorded — clearing field",
			"clusterPack", cp.Name, "targetRevision", targetRevision)
		return r.clearRollbackField(ctx, cp)
	}

	logger.Info("handleRollback: rolling back ClusterPack",
		"clusterPack", cp.Name, "fromVersion", cp.Spec.Version,
		"toVersion", targetVersion, "targetRevision", targetRevision)

	// Patch spec back to target version + digests, clear rollbackToRevision,
	// and remove the spec-snapshot annotation so the immutability check re-records
	// the rolled-back state on the next reconcile pass.
	const specSnapshotAnnotation = "infrastructure.ontai.dev/spec-checksum-snapshot"
	patch := client.MergeFrom(cp.DeepCopy())
	cp.Spec.Version = targetVersion
	cp.Spec.RBACDigest = targetRBACDigest
	cp.Spec.WorkloadDigest = targetWorkloadDigest
	cp.Spec.RollbackToRevision = 0
	if cp.Annotations != nil {
		delete(cp.Annotations, specSnapshotAnnotation)
	}
	if err := r.Client.Patch(ctx, cp, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("handleRollback: patch ClusterPack spec: %w", err)
	}

	r.Recorder.Eventf(cp, nil, corev1.EventTypeNormal, "RollbackApplied", "RollbackApplied",
		"ClusterPack rolled back from %s to %s (POR revision %d).", cp.Spec.Version, targetVersion, targetRevision)
	logger.Info("handleRollback: spec patched, normal reconcile will create PackExecution for rolled-back version",
		"clusterPack", cp.Name, "version", targetVersion)
	return ctrl.Result{}, nil
}

// clearRollbackField resets spec.rollbackToRevision to 0 when rollback cannot proceed.
func (r *ClusterPackReconciler) clearRollbackField(ctx context.Context, cp *seamcorev1alpha1.InfrastructureClusterPack) (ctrl.Result, error) {
	patch := client.MergeFrom(cp.DeepCopy())
	cp.Spec.RollbackToRevision = 0
	if err := r.Client.Patch(ctx, cp, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("clearRollbackField: patch ClusterPack: %w", err)
	}
	return ctrl.Result{}, nil
}

// handleClusterPackDeletion runs the cleanup sequence for a ClusterPack that has
// a non-zero DeletionTimestamp:
//  1. Delete all PackInstances whose spec.clusterPackRef matches cp.Name.
//  2. Delete all PackExecutions whose spec.clusterPackRef.name matches cp.Name
//     (listed cluster-wide, filtered in memory — no field index registered).
//  3. Remove the clusterPackFinalizer so the API server can delete the object.
func (r *ClusterPackReconciler) handleClusterPackDeletion(ctx context.Context, cp *seamcorev1alpha1.InfrastructureClusterPack) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Delete PackInstances matching spec.clusterPackRef == cp.Name.
	// PackInstances have no registered field index on spec.clusterPackRef, so list
	// all PackInstances cluster-wide and filter in memory.
	piList := &seamcorev1alpha1.InfrastructurePackInstanceList{}
	if err := r.Client.List(ctx, piList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list PackInstances for ClusterPack %s cleanup: %w", cp.Name, err)
	}
	for i := range piList.Items {
		pi := &piList.Items[i]
		if pi.Spec.ClusterPackRef != cp.Name {
			continue
		}
		if err := r.Client.Delete(ctx, pi); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete PackInstance %s/%s: %w", pi.Namespace, pi.Name, err)
		}
		logger.Info("deleted PackInstance during ClusterPack cleanup",
			"packInstance", pi.Name, "namespace", pi.Namespace, "clusterPack", cp.Name)
	}

	// 2. Delete PackExecutions whose spec.clusterPackRef.name matches cp.Name.
	// PackExecution has no registered field index on spec.clusterPackRef.name, so list
	// all cluster-wide and filter in memory. wrapper-schema.md §3 delivery chain.
	peList := &seamcorev1alpha1.InfrastructurePackExecutionList{}
	if err := r.Client.List(ctx, peList); err != nil {
		return ctrl.Result{}, fmt.Errorf("list PackExecutions for ClusterPack %s cleanup: %w", cp.Name, err)
	}
	for i := range peList.Items {
		pe := &peList.Items[i]
		if pe.Spec.ClusterPackRef.Name != cp.Name {
			continue
		}
		if err := r.Client.Delete(ctx, pe); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete PackExecution %s/%s: %w", pe.Namespace, pe.Name, err)
		}
		logger.Info("deleted PackExecution during ClusterPack cleanup",
			"packExecution", pe.Name, "namespace", pe.Namespace, "clusterPack", cp.Name)
	}

	// 2.5. Delete DriftSignals for each target cluster.
	// Convention: DriftSignal name = "drift-{cp.Name}", namespace = "seam-tenant-{clusterName}".
	for _, clusterName := range cp.Spec.TargetClusters {
		tenantNS := "seam-tenant-" + clusterName
		signalName := "drift-" + cp.Name
		signal := &seamcorev1alpha1.DriftSignal{
			ObjectMeta: metav1.ObjectMeta{Name: signalName, Namespace: tenantNS},
		}
		if err := r.Client.Delete(ctx, signal); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete DriftSignal %s/%s: %w", tenantNS, signalName, err)
		}
		logger.Info("deleted DriftSignal during ClusterPack cleanup",
			"driftSignal", signalName, "namespace", tenantNS, "clusterPack", cp.Name)
	}

	// 3. Remove the finalizer so the API server can delete the ClusterPack object.
	cp.Finalizers = removeString(cp.Finalizers, clusterPackFinalizer)
	if err := r.Client.Update(ctx, cp); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove ClusterPack finalizer: %w", err)
	}
	logger.Info("ClusterPack cleanup complete — finalizer removed", "clusterPack", cp.Name)
	return ctrl.Result{}, nil
}

// containsString reports whether slice contains s.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// removeString returns a copy of slice with all occurrences of s removed.
func removeString(slice []string, s string) []string {
	out := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// SetupWithManager registers ClusterPackReconciler as the controller for ClusterPack.
//
// Predicate on the primary source: OR of GenerationChangedPredicate and
// AnnotationChangedPredicate. GenerationChangedPredicate alone would miss
// conductor signature annotation writes (which don't bump generation) and
// manual retrigger annotations applied by operators.
//
// Watches PackInstance with a delete-only predicate. When a PackInstance is
// deleted, the reconciler is notified so it can check whether redelivery is
// needed and create a fresh PackExecution.
func (r *ClusterPackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	packInstanceDeletePredicate := predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return false },
		UpdateFunc:  func(_ event.UpdateEvent) bool { return false },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&seamcorev1alpha1.InfrastructureClusterPack{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&batchv1.Job{}).
		Watches(&seamcorev1alpha1.InfrastructurePackInstance{},
			handler.EnqueueRequestsFromMapFunc(r.MapPackInstanceToClusterPack),
			builder.WithPredicates(packInstanceDeletePredicate),
		).
		Complete(r)
}

// MapPackInstanceToClusterPack maps a PackInstance delete event to the ClusterPack it
// references. Before returning the reconcile request, deletes the corresponding
// PackExecution so the ClusterPackReconciler creates a fresh one for redelivery
// (WS2). The deletion is best-effort -- failure is non-fatal; the reconciler handles
// the case where the PackExecution still exists (it skips creation until the stale
// object is gone or the version matches).
func (r *ClusterPackReconciler) MapPackInstanceToClusterPack(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	pi, ok := obj.(*seamcorev1alpha1.InfrastructurePackInstance)
	if !ok {
		return nil
	}
	cpName := pi.Spec.ClusterPackRef
	if cpName == "" {
		return nil
	}

	// WS2: Delete the corresponding PackExecution so the ClusterPackReconciler
	// creates a fresh one on the next reconcile pass.
	ns := pi.GetNamespace()
	clusterName := strings.TrimPrefix(ns, "seam-tenant-")
	if clusterName != ns {
		peName := cpName + "-" + clusterName
		pe := &seamcorev1alpha1.InfrastructurePackExecution{}
		if getErr := r.Client.Get(ctx, client.ObjectKey{Name: peName, Namespace: ns}, pe); getErr == nil {
			_ = r.Client.Delete(ctx, pe)
		}
	}

	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: cpName, Namespace: pi.Namespace}},
	}
}
