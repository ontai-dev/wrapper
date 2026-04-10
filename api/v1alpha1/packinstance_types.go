package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ontai-dev/seam-core/pkg/lineage"
)

// DriftPolicy controls how dependency drift is handled by the PackInstanceReconciler.
// wrapper-schema.md §3 PackInstance spec.dependencyPolicy.onDrift.
type DriftPolicy string

const (
	// DriftPolicyBlock stops all further pack ops on this cluster when a
	// dependency PackInstance reports Drifted=True.
	DriftPolicyBlock DriftPolicy = "Block"

	// DriftPolicyWarn emits a warning event when a dependency PackInstance
	// reports Drifted=True but does not block further pack ops.
	DriftPolicyWarn DriftPolicy = "Warn"

	// DriftPolicyIgnore takes no action when a dependency PackInstance reports
	// Drifted=True.
	DriftPolicyIgnore DriftPolicy = "Ignore"
)

// Condition type constants for PackInstance. Used in status.Conditions[].Type.
// wrapper-schema.md §3 PackInstance.
const (
	// ConditionTypePackInstanceReady indicates the pack has been delivered and
	// is in the expected state on the target cluster.
	ConditionTypePackInstanceReady = "Ready"

	// ConditionTypePackInstanceProgressing indicates the pack is being applied
	// or is mid-deployment.
	ConditionTypePackInstanceProgressing = "Progressing"

	// ConditionTypePackInstanceDrifted indicates the conductor drift detection
	// loop has detected a divergence between desired and actual pack state.
	ConditionTypePackInstanceDrifted = "Drifted"

	// ConditionTypePackInstanceDependencyBlocked indicates a dependency
	// PackInstance has Drifted=True and the DriftPolicy is Block.
	ConditionTypePackInstanceDependencyBlocked = "DependencyBlocked"

	// ConditionTypePackInstanceSecurityViolation indicates the PackReceipt
	// from the target cluster reports signatureVerified=false. All further
	// pack operations on this cluster are blocked until resolved.
	ConditionTypePackInstanceSecurityViolation = "SecurityViolation"
)

// Condition reason constants for PackInstance.
const (
	ReasonPackDelivered            = "PackDelivered"
	ReasonDriftDetected            = "DriftDetected"
	ReasonNoDrift                  = "NoDrift"
	ReasonDependencyDrifted        = "DependencyDrifted"
	ReasonSignatureVerifyFailed    = "SignatureVerifyFailed"
	ReasonPackReceiptNotFound      = "PackReceiptNotFound"
	ReasonPackReceiptReady         = "PackReceiptReady"
	ReasonSecurityViolationCleared = "SecurityViolationCleared"
	// ReasonAwaitingDelivery is set on Ready=False when no succeeded PackExecution
	// exists for the pack+cluster pair. DSNSReconciler in seam-core waits for
	// Ready=True before emitting the pack DNS TXT record.
	ReasonAwaitingDelivery = "AwaitingDelivery"
)

// DependencyPolicy defines behavior when a dependency PackInstance reports drift.
// wrapper-schema.md §3 PackInstance spec.dependencyPolicy.
type DependencyPolicy struct {
	// OnDrift controls how this PackInstance responds when a declared dependency
	// PackInstance reports Drifted=True.
	// +kubebuilder:validation:Enum=Block;Warn;Ignore
	// +kubebuilder:default=Warn
	OnDrift DriftPolicy `json:"onDrift,omitempty"`
}

// PackInstanceSpec defines the desired state of a PackInstance.
// wrapper-schema.md §3.
type PackInstanceSpec struct {
	// ClusterPackRef identifies the ClusterPack that was delivered to produce
	// this instance record.
	// +kubebuilder:validation:MinLength=1
	ClusterPackRef string `json:"clusterPackRef"`

	// TargetClusterRef is the name of the TalosCluster CR to which this pack
	// has been delivered.
	// +kubebuilder:validation:MinLength=1
	TargetClusterRef string `json:"targetClusterRef"`

	// DependsOn is the list of PackInstance names that this instance depends on.
	// The DependencyPolicy.OnDrift setting governs behavior when any listed
	// dependency reports drift.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// DependencyPolicy defines behavior when a dependency reports drift.
	// +optional
	DependencyPolicy *DependencyPolicy `json:"dependencyPolicy,omitempty"`

	// Lineage is the sealed causal chain record for this root declaration.
	// Authored once at object creation time and immutable thereafter.
	// seam-core-schema.md §5, CLAUDE.md §14 Decision 1.
	// +optional
	Lineage *lineage.SealedCausalChain `json:"lineage,omitempty"`
}

// PackInstanceStatus defines the observed state of a PackInstance.
type PackInstanceStatus struct {
	// ObservedGeneration is the generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DeliveredAt records when the pack was most recently confirmed delivered
	// (PackReceipt observed in a Ready state).
	// +optional
	DeliveredAt *metav1.Time `json:"deliveredAt,omitempty"`

	// DriftSummary is a human-readable one-line summary of the current drift
	// state as reported by the conductor drift detection loop via PackReceipt.
	// +optional
	DriftSummary string `json:"driftSummary,omitempty"`

	// Conditions is the list of status conditions for this PackInstance.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PackInstance records the delivered pack state on a target cluster. It is created
// by wrapper after a successful PackExecution and updated by the drift detection loop.
// One PackInstance per pack per target cluster. wrapper-schema.md §3.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pi
// +kubebuilder:printcolumn:name="Pack",type=string,JSONPath=`.spec.clusterPackRef`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.targetClusterRef`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Drifted",type=string,JSONPath=`.status.conditions[?(@.type=="Drifted")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PackInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PackInstanceSpec   `json:"spec,omitempty"`
	Status PackInstanceStatus `json:"status,omitempty"`
}

// PackInstanceList is the list type for PackInstance.
//
// +kubebuilder:object:root=true
type PackInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []PackInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PackInstance{}, &PackInstanceList{})
}
