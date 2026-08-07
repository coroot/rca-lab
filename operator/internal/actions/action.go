// Package actions implements the failure-injection primitives used by
// FailureScenario runs.
//
// The contract that makes reverts crash-safe: Plan is a read-only observation
// of live state that returns a fully self-contained revert token. The
// controller persists that token to the scenario status BEFORE the first
// mutation, and Revert converges back to normal from the token alone — it
// never reads the scenario spec.
package actions

import (
	"context"
	"encoding/json"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

// Shared labels/annotations.
const (
	// ScenarioLabel marks every object created by the operator with the
	// owning FailureScenario name.
	ScenarioLabel = "rcalab.dev/scenario"
	// HeldByAnnotation is stamped on mutated (not created) targets, value
	// "<scenario>/<runID>".
	HeldByAnnotation = "rcalab.dev/held-by"
)

// RunContext carries run-scoped facts an action may need.
type RunContext struct {
	ScenarioName string
	ScenarioUID  types.UID
	Namespace    string
	RunID        string
	// RunDuration is the planned run length (0 = unbounded hold).
	RunDuration   time.Duration
	OperatorImage string
}

// Action is a failure-injection primitive. All methods are idempotent and
// non-blocking (level-triggered): callers requeue on requeueAfter>0.
type Action interface {
	Type() string
	// Plan observes live state and returns a self-contained revert token.
	// Read-only. Idempotent.
	Plan(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action) (json.RawMessage, error)
	// Ensure converges toward the injected state. Idempotent, non-blocking;
	// returns requeueAfter>0 if still progressing.
	Ensure(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action, token json.RawMessage) (done bool, requeueAfter time.Duration, err error)
	// Revert converges to normal from the token alone. Idempotent; tolerates
	// never-applied actions, vanished targets and double calls.
	Revert(ctx context.Context, c client.Client, rc RunContext, token json.RawMessage) (done bool, requeueAfter time.Duration, err error)
}

var registry = map[string]Action{}

// Register adds an action implementation to the registry (called from init).
func Register(a Action) {
	registry[a.Type()] = a
}

// Get returns the registered implementation for the given action type, or nil.
func Get(t string) Action {
	return registry[t]
}
