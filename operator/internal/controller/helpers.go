package controller

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
	"github.com/coroot/rca-lab/operator/internal/actions"
)

// validateSpec is the webhook-free safety net behind the CRD's CEL rules.
func validateSpec(spec *v1alpha1.FailureScenarioSpec) error {
	if len(spec.Actions) == 0 {
		return fmt.Errorf("spec.actions must contain at least one action")
	}
	seen := map[string]bool{}
	for i := range spec.Actions {
		a := &spec.Actions[i]
		if seen[a.Name] {
			return fmt.Errorf("duplicate action name %q", a.Name)
		}
		seen[a.Name] = true
		switch a.Type {
		case v1alpha1.ActionTypeWorkload:
			if a.Workload == nil || a.Scale != nil || a.DeployImage != nil {
				return fmt.Errorf("action %q: type Workload requires exactly the workload field", a.Name)
			}
		case v1alpha1.ActionTypeScale:
			if a.Scale == nil || a.Workload != nil || a.DeployImage != nil {
				return fmt.Errorf("action %q: type Scale requires exactly the scale field", a.Name)
			}
		case v1alpha1.ActionTypeDeployImage:
			if a.DeployImage == nil || a.Workload != nil || a.Scale != nil {
				return fmt.Errorf("action %q: type DeployImage requires exactly the deployImage field", a.Name)
			}
		default:
			return fmt.Errorf("action %q: unknown type %q", a.Name, a.Type)
		}
		if actions.Get(a.Type) == nil {
			return fmt.Errorf("action %q: no implementation registered for type %q", a.Name, a.Type)
		}
	}
	return nil
}

// desiredRun decides whether a new run should start. Returns trigger=="" when
// nothing is wanted. Sticky enabled wins over a pending manual trigger.
func desiredRun(fs *v1alpha1.FailureScenario) (trigger, runID string, duration time.Duration) {
	if fs.Spec.Duration != nil {
		duration = fs.Spec.Duration.Duration
	}
	if fs.Spec.Enabled {
		return v1alpha1.RunTriggerEnabled, uuid.NewString(), duration
	}
	t := fs.Spec.Trigger
	if t != nil && t.RunID != "" && t.RunID != fs.Status.LastCompletedRunID {
		if t.Duration != nil {
			duration = t.Duration.Duration
		}
		return v1alpha1.RunTriggerManual, t.RunID, duration
	}
	return "", "", 0
}

// runStillWanted reports whether the in-flight run is still requested by the
// spec (expiry is checked separately).
func runStillWanted(fs *v1alpha1.FailureScenario, run *v1alpha1.CurrentRun) bool {
	switch run.Trigger {
	case v1alpha1.RunTriggerEnabled:
		return fs.Spec.Enabled
	case v1alpha1.RunTriggerManual:
		return fs.Spec.Trigger != nil && fs.Spec.Trigger.RunID == run.ID
	}
	return false
}

func findActiveAction(list []v1alpha1.ActiveAction, name string) int {
	for i := range list {
		if list[i].Name == name {
			return i
		}
	}
	return -1
}

// statusMutation returns a patchStatus mutation targeting one active action by
// name (index-safe against concurrent status changes).
func statusMutation(name string, f func(*v1alpha1.ActiveAction)) func(*v1alpha1.FailureScenarioStatus) {
	return func(s *v1alpha1.FailureScenarioStatus) {
		if i := findActiveAction(s.ActiveActions, name); i >= 0 {
			f(&s.ActiveActions[i])
		}
	}
}

// appendHistory appends an entry, keeping the most recent historyCap entries.
func appendHistory(history []v1alpha1.RunHistoryEntry, entry v1alpha1.RunHistoryEntry) []v1alpha1.RunHistoryEntry {
	history = append(history, entry)
	if len(history) > historyCap {
		history = history[len(history)-historyCap:]
	}
	return history
}

// lowerRequeue lowers *cur to d (0 means "unset").
func lowerRequeue(cur *time.Duration, d time.Duration) {
	if d <= 0 {
		return
	}
	if *cur == 0 || d < *cur {
		*cur = d
	}
}

// backoffFor returns an exponential backoff capped at 30s.
func backoffFor(attempts int32) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 5 {
		attempts = 5
	}
	return min(30*time.Second, time.Second<<uint(attempts-1))
}

func setCondition(s *v1alpha1.FailureScenarioStatus, generation int64, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&s.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

func apiJSON(raw []byte) apiextensionsv1.JSON {
	return apiextensionsv1.JSON{Raw: raw}
}
