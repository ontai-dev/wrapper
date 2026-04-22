package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ontai-dev/seam-core/pkg/lineage"
)

// Condition type constants for ClusterPack. Used in status.Conditions[].Type.
const (
	// ConditionTypeClusterPackAvailable indicates the ClusterPack is signed and
	// ready for use by PackExecution. True = available. False = not available.
	ConditionTypeClusterPackAvailable = "Available"

	// ConditionTypeClusterPackRevoked indicates the ClusterPack has been revoked
	// and must not be used by any new PackExecution. True = revoked.
	ConditionTypeClusterPackRevoked = "Revoked"

	// ConditionTypeClusterPackSignaturePending indicates the pack is awaiting
	// conductor signing. True = signature pending.
	ConditionTypeClusterPackSignaturePending = "SignaturePending"

	// ConditionTypeClusterPackImmutabilityViolation indicates a mutation was
	// attempted on an immutable spec field. True = violation detected.
	ConditionTypeClusterPackImmutabilityViolation = "ImmutabilityViolation"
)

// Condition reason constants for ClusterPack.
const (
	ReasonPackSignaturePending  = "SignaturePending"
	ReasonPackSigned            = "PackSigned"
	ReasonPackRevoked           = "PackRevoked"
	ReasonImmutabilityViolation = "ImmutabilityViolation"
	ReasonPackAvailable         = "PackAvailable"
)

// PackRegistryRef identifies the OCI artifact for a ClusterPack.
// wrapper-schema.md §3 ClusterPack.
type PackRegistryRef struct {
	// URL is the OCI registry URL including image name (e.g., registry.ontai.dev/packs/my-pack).
	URL string `json:"url"`

	// Digest is the OCI image digest (e.g., sha256:abc123...). Immutable after creation.
	Digest string `json:"digest"`
}

// ExecutionStage represents a single stage in the pack execution order.
// wrapper-schema.md §3 ClusterPack spec.executionOrder.
type ExecutionStage struct {
	// Name is the stage name. Must be one of: rbac, storage, stateful, stateless.
	// +kubebuilder:validation:Enum=rbac;storage;stateful;stateless
	Name string `json:"name"`

	// Manifests is the list of manifest names to apply in this stage.
	// +optional
	Manifests []string `json:"manifests,omitempty"`
}

// PackProvenance records the build-time provenance of the ClusterPack artifact.
// wrapper-schema.md §3 ClusterPack spec.provenance.
type PackProvenance struct {
	// BuildID is the CI/CD build identifier that produced this pack.
	// +optional
	BuildID string `json:"buildID,omitempty"`

	// SourceRef is the git reference (commit SHA or tag) from which the pack was built.
	// +optional
	SourceRef string `json:"sourceRef,omitempty"`

	// BuildTimestamp is when the pack artifact was produced.
	// +optional
	BuildTimestamp *metav1.Time `json:"buildTimestamp,omitempty"`
}

// LifecyclePolicy defines the retain/delete policy for the ClusterPack artifact.
// wrapper-schema.md §3 ClusterPack spec.lifecyclePolicies.
type LifecyclePolicy struct {
	// RetainOnDeletion controls whether the OCI artifact is retained when the
	// ClusterPack CR is deleted. Default: true (artifact retained).
	// +optional
	// +kubebuilder:default=true
	RetainOnDeletion bool `json:"retainOnDeletion,omitempty"`
}

// ClusterPackSpec defines the desired state of a ClusterPack.
// All fields except lineage are immutable after creation. CI-INV-002.
// wrapper-schema.md §3.
type ClusterPackSpec struct {
	// Version is the semantic version of this pack. Immutable after creation.
	// Must be a valid semver string (e.g., v1.2.3).
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// RegistryRef identifies the OCI artifact for this pack. Immutable after creation.
	RegistryRef PackRegistryRef `json:"registryRef"`

	// Checksum is the SHA256 checksum of the pack artifact, used for integrity
	// verification before deployment. Immutable after creation.
	// +kubebuilder:validation:MinLength=1
	Checksum string `json:"checksum"`

	// SourceBuildRef is an opaque reference to the build that produced this pack
	// (e.g., a PackBuild CR name or CI job ID). Informational only. Immutable.
	// +optional
	SourceBuildRef string `json:"sourceBuildRef,omitempty"`

	// ExecutionOrder defines the ordered stages in which pack manifests are applied.
	// Stages must be: rbac, storage, stateful, stateless (in that order when present).
	// Immutable after creation.
	// +optional
	ExecutionOrder []ExecutionStage `json:"executionOrder,omitempty"`

	// LifecyclePolicies controls artifact retention behavior.
	// +optional
	LifecyclePolicies *LifecyclePolicy `json:"lifecyclePolicies,omitempty"`

	// Provenance records build-time metadata for audit and traceability.
	// +optional
	Provenance *PackProvenance `json:"provenance,omitempty"`

	// RBACDigest is the OCI digest of the RBAC layer of this ClusterPack artifact.
	// Contains ServiceAccount, Role, ClusterRole, RoleBinding, ClusterRoleBinding
	// manifests extracted at compile time. Guardian /rbac-intake processes this layer
	// before workload apply proceeds. Absent on pre-split ClusterPack specs.
	// wrapper-schema.md §4.
	// +optional
	RBACDigest string `json:"rbacDigest,omitempty"`

	// WorkloadDigest is the OCI digest of the workload layer of this ClusterPack
	// artifact. Contains all non-RBAC manifests. Applied after guardian RBACProfile
	// for this pack reaches provisioned=true. Absent on pre-split ClusterPack specs.
	// wrapper-schema.md §4.
	// +optional
	WorkloadDigest string `json:"workloadDigest,omitempty"`

	// ClusterScopedDigest is the OCI digest of the cluster-scoped non-RBAC layer.
	// Contains MutatingWebhookConfiguration, ValidatingWebhookConfiguration,
	// CustomResourceDefinition, APIService, PriorityClass, StorageClass,
	// IngressClass, ClusterIssuer, and similar cluster-scoped resources.
	// Applied after guardian RBAC intake and before workload manifests.
	// Absent when the chart has no cluster-scoped resources. wrapper-schema.md §4.
	// +optional
	ClusterScopedDigest string `json:"clusterScopedDigest,omitempty"`

	// BasePackName is the logical pack name shared across versions (e.g., "nginx-ingress").
	// Separate from the versioned CR name (e.g., "nginx-ingress-v4.9.0-r1"). When set,
	// PackInstances are named {basePackName}-{clusterName} so that a newer version of the
	// same base pack supersedes an older one in-place rather than creating a parallel
	// PackInstance. Schema-first per Decision 11.
	// +optional
	BasePackName string `json:"basePackName,omitempty"`

	// TargetClusters is the list of cluster names to which this ClusterPack should
	// be delivered. The ClusterPackReconciler creates one RunnerConfig per entry in
	// seam-tenant-{clusterName} after signing completes. wrapper-schema.md §4.
	// +optional
	TargetClusters []string `json:"targetClusters,omitempty"`

	// Lineage is the sealed causal chain record for this root declaration.
	// Authored once at object creation time and immutable thereafter.
	// The admission webhook rejects any update that modifies this field after creation.
	// seam-core-schema.md §5, CLAUDE.md §14 Decision 1.
	// +optional
	Lineage *lineage.SealedCausalChain `json:"lineage,omitempty"`
}

// ClusterPackStatus defines the observed state of a ClusterPack.
type ClusterPackStatus struct {
	// ObservedGeneration is the generation most recently reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Signed indicates whether the conductor signing loop has signed this pack.
	// True = conductor has placed its signature annotation and updated this field.
	// +optional
	Signed bool `json:"signed,omitempty"`

	// PackSignature is the base64-encoded Ed25519 signature produced by the
	// management cluster conductor signing loop. Present only when Signed=true.
	// +optional
	PackSignature string `json:"packSignature,omitempty"`

	// Conditions is the list of status conditions for this ClusterPack.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterPack is the primary pack registration resource for the infra.ontai.dev API group.
// It records an OCI artifact that has been compiled (off-cluster) and is ready for
// runtime delivery. Once registered, ClusterPack spec is immutable. Changes require
// creating a new ClusterPack. CI-INV-002, wrapper-schema.md §3.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cp
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Signed",type=boolean,JSONPath=`.status.signed`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ClusterPack struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterPackSpec   `json:"spec,omitempty"`
	Status ClusterPackStatus `json:"status,omitempty"`
}

// ClusterPackList is the list type for ClusterPack.
//
// +kubebuilder:object:root=true
type ClusterPackList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ClusterPack `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterPack{}, &ClusterPackList{})
}
