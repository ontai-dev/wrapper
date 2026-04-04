package v1alpha1

// Lineage condition type and reason constants shared across all Wrapper CRD
// status condition sets. These values are defined by seam-core-schema.md §7
// Declaration 5 and are reserved platform-wide. No Wrapper CRD may use the
// ConditionTypeLineageSynced name for any purpose other than lineage sync tracking.

const (
	// ConditionTypeLineageSynced is the reserved condition type for lineage
	// synchronization status on every root declaration CR.
	//
	// Lifecycle:
	//  1. On first observation of a root declaration CR, the responsible reconciler
	//     sets this condition to False with reason LineageControllerAbsent.
	//     This is a one-time initialization — the reconciler never writes this
	//     condition again.
	//  2. When InfrastructureLineageController is deployed and processes the root
	//     declaration, it takes ownership of this condition and sets it to True.
	//  3. If InfrastructureLineageController is not deployed, this condition remains
	//     False/LineageControllerAbsent indefinitely. This is the expected steady
	//     state during the stub phase and is not an error condition.
	//
	// seam-core-schema.md §7 Declaration 5.
	ConditionTypeLineageSynced = "LineageSynced"

	// ReasonLineageControllerAbsent is set when a reconciler initializes the
	// LineageSynced condition to False. It indicates that InfrastructureLineageController
	// has not yet been deployed and has not processed this root declaration.
	// seam-core-schema.md §7 Declaration 5.
	ReasonLineageControllerAbsent = "LineageControllerAbsent"
)
