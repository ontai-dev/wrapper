package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ontai-dev/seam-core/pkg/lineage"
)

// Condition type constants for PackExecution. Used in status.Conditions[].Type.
// wrapper-schema.md §3 PackExecution.
const (
	// ConditionTypePackExecutionPending indicates the PackExecution is waiting
	// for gate conditions to clear before Job submission.
	ConditionTypePackExecutionPending = "Pending"

	// ConditionTypePackSignaturePending indicates the referenced ClusterPack
	// has not yet been signed by the conductor signing loop.
	ConditionTypePackSignaturePending = "PackSignaturePending"

	// ConditionTypePackExecutionRunning indicates the pack-deploy Job has been
	// submitted to Kueue and is running.
	ConditionTypePackExecutionRunning = "Running"

	// ConditionTypePackExecutionSucceeded indicates the pack-deploy Job completed
	// successfully and OperationResult was written.
	ConditionTypePackExecutionSucceeded = "Succeeded"

	// ConditionTypePackExecutionFailed indicates the pack-deploy Job failed.
	// Human intervention is required to investigate and retry.
	ConditionTypePackExecutionFailed = "Failed"

	// ConditionTypePackRevoked indicates the referenced ClusterPack has been
	// revoked. No requeue is issued — human intervention is required.
	ConditionTypePackRevoked = "PackRevoked"

	// ConditionTypePermissionSnapshotOutOfSync indicates the PermissionSnapshot
	// for the target cluster is not current. Requeue with backoff.
	ConditionTypePermissionSnapshotOutOfSync = "PermissionSnapshotOutOfSync"

	// ConditionTypeRBACProfileNotProvisioned indicates the RBACProfile for the
	// target cluster has not reached provisioned=true. Requeue with backoff.
	ConditionTypeRBACProfileNotProvisioned = "RBACProfileNotProvisioned"

	// ConditionTypePackExecutionWaiting indicates the PackExecution is waiting for
	// a cluster-level prerequisite before any pack-level gates are checked.
	// Currently used for the ConductorReady gate (gate 0). wrapper-schema.md §4,
	// platform-schema.md §12. Gap 27.
	ConditionTypePackExecutionWaiting = "Waiting"
)

// Condition reason constants for PackExecution.
const (
	ReasonAwaitingSignature        = "AwaitingSignature"
	ReasonClusterPackRevoked       = "ClusterPackRevoked"
	ReasonSnapshotOutOfSync        = "SnapshotOutOfSync"
	ReasonRBACProfileNotReady      = "RBACProfileNotProvisioned"
	ReasonJobSubmitted             = "JobSubmitted"
	ReasonJobSucceeded             = "JobSucceeded"
	ReasonJobFailed                = "JobFailed"
	ReasonGatesClearing            = "GatesClearing"
	ReasonOperationResultNotFound  = "OperationResultNotFound"
	// ReasonAwaitingConductorReady is set on Waiting when the target cluster
	// TalosCluster does not yet have ConductorReady=True. platform-schema.md §12.
	ReasonAwaitingConductorReady   = "AwaitingConductorReady"
)

// ClusterPackRef identifies a specific ClusterPack by name and version.
// wrapper-schema.md §3 PackExecution spec.clusterPackRef.
type ClusterPackRef struct {
	// Name is the name of the ClusterPack CR.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the expected version of the ClusterPack. Used to guard against
	// race conditions where the name is reused with a different version.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
}

// PackExecutionSpec defines the desired state of a PackExecution.
// wrapper-schema.md §3.
type PackExecutionSpec struct {
	// ClusterPackRef identifies the ClusterPack to deploy.
	ClusterPackRef ClusterPackRef `json:"clusterPackRef"`

	// TargetClusterRef is the name of the TalosCluster CR that is the deployment target.
	// The cluster must be Ready before the pack-deploy Job will be admitted.
	// +kubebuilder:validation:MinLength=1
	TargetClusterRef string `json:"targetClusterRef"`

	// AdmissionProfileRef is the name of the RBACProfile that governs this execution.
	// The 4-gate check verifies this profile has reached provisioned=true.
	// +kubebuilder:validation:MinLength=1
	AdmissionProfileRef string `json:"admissionProfileRef"`

	// Lineage is the sealed causal chain record for this root declaration.
	// Authored once at object creation time and immutable thereafter.
	// seam-core-schema.md §5, CLAUDE.md §14 Decision 1.
	// +optional
	Lineage *lineage.SealedCausalChain `json:"lineage,omitempty"`
}

// PackExecutionStatus defines the observed state of a PackExecution.
type PackExecutionStatus struct {
	// ObservedGeneration is the generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// JobName is the name of the pack-deploy Kueue Job that was submitted.
	// Present after Job submission.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// OperationResultRef is the name of the ConfigMap written by the conductor
	// executor after successful pack-deploy Job completion.
	// +optional
	OperationResultRef string `json:"operationResultRef,omitempty"`

	// Conditions is the list of status conditions for this PackExecution.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PackExecution is the runtime request to apply a ClusterPack to a target cluster.
// The reconciler performs a 4-gate check before submitting a pack-deploy Job via Kueue.
// wrapper-schema.md §3.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pe
// +kubebuilder:printcolumn:name="Pack",type=string,JSONPath=`.spec.clusterPackRef.name`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.targetClusterRef`
// +kubebuilder:printcolumn:name="Running",type=string,JSONPath=`.status.conditions[?(@.type=="Running")].status`
// +kubebuilder:printcolumn:name="Succeeded",type=string,JSONPath=`.status.conditions[?(@.type=="Succeeded")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PackExecution struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PackExecutionSpec   `json:"spec,omitempty"`
	Status PackExecutionStatus `json:"status,omitempty"`
}

// PackExecutionList is the list type for PackExecution.
//
// +kubebuilder:object:root=true
type PackExecutionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PackExecution `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PackExecution{}, &PackExecutionList{})
}
