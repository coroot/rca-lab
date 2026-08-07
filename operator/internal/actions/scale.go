package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

func init() {
	Register(&Scale{})
}

// ScaleToken is the self-contained revert token of a Scale action.
type ScaleToken struct {
	Kind             string `json:"kind"`
	Namespace        string `json:"namespace"`
	Name             string `json:"name"`
	PreviousReplicas int32  `json:"previousReplicas"`
}

// Scale changes the replica count of a Deployment.
type Scale struct{}

func (s *Scale) Type() string { return v1alpha1.ActionTypeScale }

func (s *Scale) Plan(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action) (json.RawMessage, error) {
	sa := spec.Scale
	if sa == nil {
		return nil, fmt.Errorf("action %q: scale config missing", spec.Name)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: rc.Namespace, Name: sa.TargetRef.Name}, dep); err != nil {
		return nil, fmt.Errorf("scale target deployment %s/%s: %w", rc.Namespace, sa.TargetRef.Name, err)
	}
	prev := int32(1)
	if dep.Spec.Replicas != nil {
		prev = *dep.Spec.Replicas
	}
	return json.Marshal(ScaleToken{Kind: "Deployment", Namespace: rc.Namespace, Name: sa.TargetRef.Name, PreviousReplicas: prev})
}

func (s *Scale) Ensure(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action, token json.RawMessage) (bool, time.Duration, error) {
	var t ScaleToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid scale token: %w", err)
	}
	sa := spec.Scale
	if sa == nil {
		return false, 0, fmt.Errorf("action %q: scale config missing", spec.Name)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, dep); err != nil {
		return false, 0, err
	}

	heldBy := rc.ScenarioName + "/" + rc.RunID
	if cur := dep.Annotations[HeldByAnnotation]; cur != "" && !strings.HasPrefix(cur, rc.ScenarioName+"/") {
		otherName, _, _ := strings.Cut(cur, "/")
		other := &v1alpha1.FailureScenario{}
		err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: otherName}, other)
		switch {
		case err == nil && (other.Status.CurrentRun != nil || len(other.Status.ActiveActions) > 0):
			return false, 0, fmt.Errorf("deployment %s/%s is held by live scenario %q", t.Namespace, t.Name, cur)
		case err != nil && !errors.IsNotFound(err):
			return false, 0, err
		}
		// The holder is gone or idle: take over its stale annotation.
	}

	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == sa.Replicas && dep.Annotations[HeldByAnnotation] == heldBy {
		return true, 0, nil
	}
	patched := dep.DeepCopy()
	patched.Spec.Replicas = ptr.To(sa.Replicas)
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[HeldByAnnotation] = heldBy
	if err := c.Patch(ctx, patched, client.MergeFrom(dep)); err != nil {
		return false, 0, err
	}
	// Keep it simple: done right after the patch (the Deployment controller
	// converges asynchronously).
	return true, 0, nil
}

func (s *Scale) Revert(ctx context.Context, c client.Client, rc RunContext, token json.RawMessage) (bool, time.Duration, error) {
	var t ScaleToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid scale token: %w", err)
	}
	dep := &appsv1.Deployment{}
	err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, dep)
	if errors.IsNotFound(err) {
		return true, 0, nil // target vanished: nothing to restore
	}
	if err != nil {
		return false, 0, err
	}
	holder := dep.Annotations[HeldByAnnotation]
	if holder != "" && !strings.HasPrefix(holder, rc.ScenarioName+"/") {
		// Held by a different scenario: our Ensure never applied (the hold
		// check rejects foreign holders), so there is nothing to restore —
		// and the foreign lock must not be disturbed.
		return true, 0, nil
	}
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas == t.PreviousReplicas && holder == "" {
		return true, 0, nil // already normal (never applied or double revert)
	}
	patched := dep.DeepCopy()
	patched.Spec.Replicas = ptr.To(t.PreviousReplicas)
	delete(patched.Annotations, HeldByAnnotation)
	if err := c.Patch(ctx, patched, client.MergeFrom(dep)); err != nil {
		return false, 0, err
	}
	return true, 0, nil
}
