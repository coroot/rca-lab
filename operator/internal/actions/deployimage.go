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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

func init() {
	Register(&DeployImage{})
}

const (
	// KnownGoodAnnotationPrefix marks a mutated Deployment with its rollback
	// image (suffix = container name). It survives even a force-deleted
	// scenario, so the sweeper and humans can always find the way back.
	KnownGoodAnnotationPrefix = "rcalab.dev/known-good."

	defaultRolloutTimeout = 5 * time.Minute
)

// DeployImageToken is the self-contained revert token of a DeployImage action.
type DeployImageToken struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Container string `json:"container"`
	// GoodImage is the rollback target resolved at plan time.
	GoodImage string `json:"goodImage"`
	// BadImage is what the action deploys (lets Revert distinguish "our bad
	// image is live" from "someone deployed something else meanwhile").
	BadImage string `json:"badImage"`
}

// DeployImage performs a genuine version rollout of a regression variant and
// rolls it back on revert.
type DeployImage struct{}

func (d *DeployImage) Type() string { return v1alpha1.ActionTypeDeployImage }

func (d *DeployImage) Plan(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action) (json.RawMessage, error) {
	da := spec.DeployImage
	if da == nil {
		return nil, fmt.Errorf("action %q: deployImage config missing", spec.Name)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: rc.Namespace, Name: da.TargetRef.Name}, dep); err != nil {
		return nil, fmt.Errorf("deploy target %s/%s: %w", rc.Namespace, da.TargetRef.Name, err)
	}
	observed := ""
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == da.Container {
			observed = dep.Spec.Template.Spec.Containers[i].Image
			break
		}
	}
	if observed == "" {
		return nil, fmt.Errorf("deployment %s has no container %q", da.TargetRef.Name, da.Container)
	}

	// Resolve the rollback target: spec wins, then the known-good annotation
	// (a previous interrupted run), then the observed image — but never the
	// bad image itself.
	good := da.KnownGoodImage
	if good == "" {
		good = dep.Annotations[KnownGoodAnnotationPrefix+da.Container]
	}
	if good == "" {
		good = observed
	}
	if good == da.Image {
		return nil, fmt.Errorf("action %q: cannot determine a known-good image distinct from %q; set spec knownGoodImage", spec.Name, da.Image)
	}
	return json.Marshal(DeployImageToken{
		Kind: "Deployment", Namespace: rc.Namespace, Name: da.TargetRef.Name,
		Container: da.Container, GoodImage: good, BadImage: da.Image,
	})
}

func (d *DeployImage) Ensure(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action, token json.RawMessage) (bool, time.Duration, error) {
	var t DeployImageToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid deployImage token: %w", err)
	}
	da := spec.DeployImage
	if da == nil {
		return false, 0, fmt.Errorf("action %q: deployImage config missing", spec.Name)
	}
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, dep); err != nil {
		return false, 0, err
	}

	if cur := dep.Annotations[HeldByAnnotation]; cur != "" && !strings.HasPrefix(cur, rc.ScenarioName+"/") {
		return false, 0, fmt.Errorf("deployment %s/%s is held by %q", t.Namespace, t.Name, cur)
	}

	idx := containerIndex(dep, t.Container)
	if idx < 0 {
		return false, 0, fmt.Errorf("deployment %s has no container %q", t.Name, t.Container)
	}

	if dep.Spec.Template.Spec.Containers[idx].Image != t.BadImage {
		patched := dep.DeepCopy()
		if patched.Annotations == nil {
			patched.Annotations = map[string]string{}
		}
		// The annotation and the image swap land in one patch: there is no
		// window where the bad image is live without its rollback marker.
		patched.Annotations[KnownGoodAnnotationPrefix+t.Container] = t.GoodImage
		patched.Annotations[HeldByAnnotation] = rc.ScenarioName + "/" + rc.RunID
		patched.Labels = withLabel(patched.Labels, ScenarioLabel, rc.ScenarioName)
		patched.Spec.Template.Spec.Containers[idx].Image = t.BadImage
		if err := c.Patch(ctx, patched, client.MergeFrom(dep)); err != nil {
			return false, 0, err
		}
		return false, 5 * time.Second, nil // observe the rollout next pass
	}

	// Image is set; report applied once the rollout finishes OR the timeout
	// passes. A stalled/crash-looping rollout is a legitimate failure
	// scenario, not an activation error.
	timeout := defaultRolloutTimeout
	if da.RolloutTimeout != nil {
		timeout = da.RolloutTimeout.Duration
	}
	if rolloutComplete(dep) || time.Since(rolloutStart(dep)) > timeout {
		return true, 0, nil
	}
	return false, 10 * time.Second, nil
}

func (d *DeployImage) Revert(ctx context.Context, c client.Client, rc RunContext, token json.RawMessage) (bool, time.Duration, error) {
	var t DeployImageToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid deployImage token: %w", err)
	}
	dep := &appsv1.Deployment{}
	err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, dep)
	if errors.IsNotFound(err) {
		return true, 0, nil // target vanished
	}
	if err != nil {
		return false, 0, err
	}

	holder := dep.Annotations[HeldByAnnotation]
	if holder != "" && !strings.HasPrefix(holder, rc.ScenarioName+"/") {
		return true, 0, nil // held by another scenario: we never applied
	}

	idx := containerIndex(dep, t.Container)
	current := ""
	if idx >= 0 {
		current = dep.Spec.Template.Spec.Containers[idx].Image
	}

	needsImageRestore := idx >= 0 && current == t.BadImage
	_, hasKnownGood := dep.Annotations[KnownGoodAnnotationPrefix+t.Container]
	if !needsImageRestore && !hasKnownGood && holder == "" {
		return true, 0, nil // never applied, already reverted, or superseded by a real deploy
	}

	patched := dep.DeepCopy()
	if needsImageRestore {
		patched.Spec.Template.Spec.Containers[idx].Image = t.GoodImage
	}
	delete(patched.Annotations, KnownGoodAnnotationPrefix+t.Container)
	delete(patched.Annotations, HeldByAnnotation)
	delete(patched.Labels, ScenarioLabel)
	if err := c.Patch(ctx, patched, client.MergeFrom(dep)); err != nil {
		return false, 0, err
	}
	if needsImageRestore {
		return false, 5 * time.Second, nil // wait for the rollback rollout
	}
	return true, 0, nil
}

func containerIndex(dep *appsv1.Deployment, name string) int {
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == name {
			return i
		}
	}
	return -1
}

func withLabel(labels map[string]string, key, value string) map[string]string {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[key] = value
	return labels
}

// rolloutComplete mirrors kubectl rollout status semantics for Deployments.
func rolloutComplete(dep *appsv1.Deployment) bool {
	if dep.Generation > dep.Status.ObservedGeneration {
		return false
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	return dep.Status.UpdatedReplicas == desired &&
		dep.Status.AvailableReplicas == desired &&
		dep.Status.Replicas == desired
}

// rolloutStart approximates when the current rollout began.
func rolloutStart(dep *appsv1.Deployment) time.Time {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing {
			return c.LastUpdateTime.Time
		}
	}
	return dep.CreationTimestamp.Time
}
