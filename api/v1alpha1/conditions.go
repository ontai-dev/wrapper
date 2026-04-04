package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetCondition finds an existing condition of the given type in the slice and
// updates it in-place, or appends a new condition if none exists.
//
// LastTransitionTime is updated only when the Status value changes, following
// the standard Kubernetes condition update pattern. This prevents spurious
// status updates when conditions are repeatedly reconciled with no change.
func SetCondition(
	conditions *[]metav1.Condition,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
	observedGeneration int64,
) {
	now := metav1.Now()
	existing := FindCondition(*conditions, conditionType)
	if existing == nil {
		*conditions = append(*conditions, metav1.Condition{
			Type:               conditionType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: observedGeneration,
			LastTransitionTime: now,
		})
		return
	}
	if existing.Status != status {
		existing.LastTransitionTime = now
	}
	existing.Status = status
	existing.Reason = reason
	existing.Message = message
	existing.ObservedGeneration = observedGeneration
}

// FindCondition returns a pointer to the first condition in the slice with the
// given Type, or nil if no such condition exists.
func FindCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
