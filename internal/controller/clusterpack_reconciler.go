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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/ontai-dev/wrapper/api/v1alpha1"
)

// packSignatureAnnotation is the annotation key set by the conductor signing loop
// on a ClusterPack CR after Ed25519 signing. The reconciler reads this annotation
// to transition the ClusterPack from SignaturePending to Available.
// INV-026: signing is management cluster conductor's responsibility only.
const packSignatureAnnotation = "ontai.dev/pack-signature"

// clusterPackFinalizer is added to every ClusterPack on first reconcile.
// On deletion it triggers cleanup of all PackInstances and RunnerConfigs
// derived from this ClusterPack before the object is removed from etcd.
const clusterPackFinalizer = "infra.ontai.dev/clusterpack-cleanup"

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
// +kubebuilder:rbac:groups=infra.ontai.dev,resources=packinstances,verbs=get;list;delete
// +kubebuilder:rbac:groups=runner.ontai.dev,resources=runnerconfigs,verbs=get;list;delete
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
		return ctrl.Result{}, nil
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

	// Step I — RunnerConfig provisioning: create one RunnerConfig per targetCluster
	// in seam-tenant-{clusterName} with a pack-deploy step. Idempotent: skips if
	// PackInstance or RunnerConfig already exists for this (pack, cluster) pair.
	// Conductor agent watches RunnerConfigs labeled infra.ontai.dev/pack and creates
	// PackExecution from them (WS3). wrapper-schema.md §9 delivery chain.
	for _, clusterName := range cp.Spec.TargetClusters {
		tenantNS := "seam-tenant-" + clusterName
		rcName := cp.Name + "-" + clusterName

		// Check whether a PackInstance already exists for this (pack, cluster) pair.
		// If it does and the version matches the current ClusterPack, delivery is
		// already complete — skip. If versions differ, an upgrade is needed: delete
		// the existing RunnerConfig (if any) so a fresh one is created below.
		existingPI := &infrav1alpha1.PackInstance{}
		piName := cp.Name + "-" + clusterName
		piErr := r.Client.Get(ctx, client.ObjectKey{Name: piName, Namespace: tenantNS}, existingPI)
		if piErr == nil {
			if existingPI.Spec.Version == cp.Spec.Version {
				logger.Info("PackInstance exists with current version — skipping RunnerConfig creation",
					"pack", cp.Name, "cluster", clusterName, "version", cp.Spec.Version)
				continue
			}
			// Version mismatch: delete the old RunnerConfig (if present) so the new
			// one created below replaces it with updated version labels.
			logger.Info("PackInstance version mismatch — triggering upgrade",
				"pack", cp.Name, "cluster", clusterName,
				"currentVersion", existingPI.Spec.Version, "targetVersion", cp.Spec.Version)
			oldRC := &unstructured.Unstructured{}
			oldRC.SetGroupVersionKind(schema.GroupVersionKind{
				Group: "runner.ontai.dev", Version: "v1alpha1", Kind: "RunnerConfig",
			})
			if err := r.Client.Get(ctx, client.ObjectKey{Name: rcName, Namespace: tenantNS}, oldRC); err == nil {
				if err := r.Client.Delete(ctx, oldRC); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("delete old RunnerConfig for upgrade %s/%s: %w", tenantNS, rcName, err)
				}
			} else if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("get RunnerConfig for upgrade check %s/%s: %w", tenantNS, rcName, err)
			}
			// Fall through to create a new RunnerConfig with the current version.
		} else if !apierrors.IsNotFound(piErr) {
			return ctrl.Result{}, fmt.Errorf("get PackInstance %s/%s: %w", tenantNS, piName, piErr)
		}

		// Skip if RunnerConfig already exists — Conductor has it in flight.
		existingRC := &unstructured.Unstructured{}
		existingRC.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "runner.ontai.dev", Version: "v1alpha1", Kind: "RunnerConfig",
		})
		if err := r.Client.Get(ctx, client.ObjectKey{Name: rcName, Namespace: tenantNS}, existingRC); err == nil {
			logger.Info("RunnerConfig exists — skipping creation",
				"pack", cp.Name, "cluster", clusterName)
			continue
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get RunnerConfig %s/%s: %w", tenantNS, rcName, err)
		}

		// Create RunnerConfig with pack-deploy step in seam-tenant-{clusterName}.
		// Labels platform.ontai.dev/cluster and infra.ontai.dev/pack enable
		// Conductor agent to find and act on this RunnerConfig. WS3.
		newRC := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "runner.ontai.dev/v1alpha1",
			"kind":       "RunnerConfig",
			"metadata": map[string]interface{}{
				"name":      rcName,
				"namespace": tenantNS,
				"labels": map[string]interface{}{
					"platform.ontai.dev/cluster":       clusterName,
					"infra.ontai.dev/pack":              cp.Name,
					"infra.ontai.dev/pack-version":      cp.Spec.Version,
					"infra.ontai.dev/admission-profile": "rbac-wrapper",
				},
			},
			"spec": map[string]interface{}{
				"clusterRef":  clusterName,
				"runnerImage": conductorImageDefault,
				"steps": []interface{}{
					map[string]interface{}{
						"name":          "pack-deploy",
						"capability":    "pack-deploy",
						"haltOnFailure": true,
					},
				},
			},
		}}
		if err := r.Client.Create(ctx, newRC); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create RunnerConfig %s/%s: %w", tenantNS, rcName, err)
		}
		logger.Info("RunnerConfig created for pack delivery",
			"pack", cp.Name, "version", cp.Spec.Version, "cluster", clusterName, "runnerConfig", rcName)
		r.Recorder.Event(cp, corev1.EventTypeNormal, "RunnerConfigCreated",
			fmt.Sprintf("RunnerConfig %s created in %s for pack delivery to cluster %s.", rcName, tenantNS, clusterName))
	}

	return ctrl.Result{}, nil
}

// handleClusterPackDeletion runs the cleanup sequence for a ClusterPack that has
// a non-zero DeletionTimestamp:
//  1. Delete all PackInstances whose spec.clusterPackRef matches cp.Name.
//  2. Delete all RunnerConfigs labeled infra.ontai.dev/pack=cp.Name across
//     seam-tenant-* namespaces (listed cluster-wide; namespace filter is applied
//     by Kubernetes label selector).
//  3. Remove the clusterPackFinalizer so the API server can delete the object.
func (r *ClusterPackReconciler) handleClusterPackDeletion(ctx context.Context, cp *infrav1alpha1.ClusterPack) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Delete PackInstances matching spec.clusterPackRef == cp.Name.
	// PackInstances have no registered field index on spec.clusterPackRef, so list
	// all PackInstances cluster-wide and filter in memory.
	piList := &infrav1alpha1.PackInstanceList{}
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

	// 2. Delete RunnerConfigs labeled infra.ontai.dev/pack=cp.Name.
	// Use unstructured list because RunnerConfig type is from an external module.
	rcList := &unstructured.UnstructuredList{}
	rcList.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "runner.ontai.dev", Version: "v1alpha1", Kind: "RunnerConfigList",
	})
	if err := r.Client.List(ctx, rcList, client.MatchingLabels{"infra.ontai.dev/pack": cp.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list RunnerConfigs for ClusterPack %s cleanup: %w", cp.Name, err)
	}
	for i := range rcList.Items {
		rc := &rcList.Items[i]
		if err := r.Client.Delete(ctx, rc); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete RunnerConfig %s/%s: %w", rc.GetNamespace(), rc.GetName(), err)
		}
		logger.Info("deleted RunnerConfig during ClusterPack cleanup",
			"runnerConfig", rc.GetName(), "namespace", rc.GetNamespace(), "clusterPack", cp.Name)
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
// needed and recreate the RunnerConfig. WS1.
func (r *ClusterPackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	packInstanceDeletePredicate := predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return false },
		UpdateFunc:  func(_ event.UpdateEvent) bool { return false },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.ClusterPack{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
		))).
		Owns(&batchv1.Job{}).
		Watches(&infrav1alpha1.PackInstance{},
			handler.EnqueueRequestsFromMapFunc(r.mapPackInstanceToClusterPack),
			builder.WithPredicates(packInstanceDeletePredicate),
		).
		Complete(r)
}

// mapPackInstanceToClusterPack maps a PackInstance event to the ClusterPack it
// references. Reads spec.clusterPackRef (the ClusterPack name) and returns a
// reconcile request for that ClusterPack in the same namespace as the PackInstance.
// Used by the WS1 delete watch so the ClusterPackReconciler re-evaluates whether
// redelivery RunnerConfig creation is needed.
func (r *ClusterPackReconciler) mapPackInstanceToClusterPack(
	_ context.Context,
	obj client.Object,
) []reconcile.Request {
	pi, ok := obj.(*infrav1alpha1.PackInstance)
	if !ok {
		return nil
	}
	cpName := pi.Spec.ClusterPackRef
	if cpName == "" {
		return nil
	}
	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: cpName, Namespace: pi.Namespace}},
	}
}
