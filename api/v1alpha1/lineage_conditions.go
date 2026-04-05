package v1alpha1

// Lineage condition type and reason constants re-exported from
// seam-core/pkg/conditions — the canonical source. seam-core-schema.md §7
// Declaration 5. SC-INV-002 / Gap 31 WS2.
//
// Wrapper reconcilers reference these via the wrapperv1alpha1 package alias;
// they continue to compile without modification. New code should prefer importing
// github.com/ontai-dev/seam-core/pkg/conditions directly.

import "github.com/ontai-dev/seam-core/pkg/conditions"

const (
	// ConditionTypeLineageSynced is the reserved condition type for lineage
	// synchronization status on every root declaration CR.
	//
	// Lifecycle (seam-core-schema.md §7 Declaration 5):
	//  1. On first observation the responsible reconciler sets this to False with
	//     reason ReasonLineageControllerAbsent. One-time write.
	//  2. InfrastructureLineageController takes ownership on deployment, sets True.
	//  3. If InfrastructureLineageController is absent, remains False/LineageControllerAbsent.
	//
	// Canonical source: github.com/ontai-dev/seam-core/pkg/conditions.
	ConditionTypeLineageSynced = conditions.ConditionTypeLineageSynced

	// ReasonLineageControllerAbsent is set when the reconciler initialises
	// LineageSynced to False. Canonical source: pkg/conditions.
	ReasonLineageControllerAbsent = conditions.ReasonLineageControllerAbsent
)
