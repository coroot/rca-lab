package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

func init() {
	Register(&ChaosMesh{})
}

const (
	// chaosMeshAPIVersion is the group/version of Chaos Mesh custom resources.
	chaosMeshAPIVersion = "chaos-mesh.org/v1alpha1"
	// chaosDeadmanMargin is added to the run duration to compute the chaos
	// object's own duration field, so it self-recovers after the run even if
	// the operator dies.
	chaosDeadmanMargin = 5 * time.Minute
	// chaosUnboundedDuration caps chaos objects of runs without a duration.
	chaosUnboundedDuration = "24h"
)

// ChaosMeshToken is the self-contained revert token of a ChaosMesh action.
type ChaosMeshToken struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
}

// ChaosMesh creates a Chaos Mesh custom resource (unstructured) and deletes it
// on revert.
type ChaosMesh struct{}

func (m *ChaosMesh) Type() string { return v1alpha1.ActionTypeChaosMesh }

func (m *ChaosMesh) Plan(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action) (json.RawMessage, error) {
	ca := spec.ChaosMesh
	if ca == nil {
		return nil, fmt.Errorf("action %q: chaosMesh config missing", spec.Name)
	}
	name := chaosObjectName(ca, rc, spec.Name)
	obj := newChaosUnstructured(ca.Kind, rc.Namespace, name)
	err := c.Get(ctx, types.NamespacedName{Namespace: rc.Namespace, Name: name}, obj)
	switch {
	case err == nil:
		if obj.GetLabels()[ScenarioLabel] != rc.ScenarioName {
			return nil, fmt.Errorf("%s %s/%s already exists and is not owned by scenario %q (name conflict)",
				ca.Kind, rc.Namespace, name, rc.ScenarioName)
		}
	case !errors.IsNotFound(err):
		return nil, err
	}
	return json.Marshal(ChaosMeshToken{
		APIVersion: chaosMeshAPIVersion,
		Kind:       ca.Kind,
		Namespace:  rc.Namespace,
		Name:       name,
	})
}

func (m *ChaosMesh) Ensure(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action, token json.RawMessage) (bool, time.Duration, error) {
	var t ChaosMeshToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid chaosMesh token: %w", err)
	}
	ca := spec.ChaosMesh
	if ca == nil {
		return false, 0, fmt.Errorf("action %q: chaosMesh config missing", spec.Name)
	}

	existing := newChaosUnstructured(t.Kind, t.Namespace, t.Name)
	err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, existing)
	switch {
	case err == nil:
		if existing.GetLabels()[ScenarioLabel] != rc.ScenarioName {
			return false, 0, fmt.Errorf("%s %s/%s exists but is not ours", t.Kind, t.Namespace, t.Name)
		}
		return true, 0, nil // exists and ours: done (don't block on chaos status)
	case !errors.IsNotFound(err):
		return false, 0, err
	}

	obj, err := m.buildChaos(rc, ca, t)
	if err != nil {
		return false, 0, err
	}
	if err := c.Create(ctx, obj); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, time.Second, nil // re-observe on next pass
		}
		return false, 0, err
	}
	return true, 0, nil
}

func (m *ChaosMesh) buildChaos(rc RunContext, ca *v1alpha1.ChaosMeshAction, t ChaosMeshToken) (*unstructured.Unstructured, error) {
	spec := map[string]any{}
	if len(ca.Spec.Raw) > 0 {
		if err := json.Unmarshal(ca.Spec.Raw, &spec); err != nil {
			return nil, fmt.Errorf("action: invalid chaosMesh spec: %w", err)
		}
	}
	// Dead-man switch: Chaos Mesh recovers the fault after .spec.duration even
	// if the operator never issues the delete. Only set it when the caller did
	// not specify one.
	if _, ok := spec["duration"]; !ok {
		spec["duration"] = chaosDuration(rc.RunDuration)
	}

	obj := newChaosUnstructured(t.Kind, t.Namespace, t.Name)
	obj.SetLabels(map[string]string{ScenarioLabel: rc.ScenarioName})
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "FailureScenario",
		Name:               rc.ScenarioName,
		UID:                rc.ScenarioUID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(false),
	}})
	obj.Object["spec"] = spec
	return obj, nil
}

func (m *ChaosMesh) Revert(ctx context.Context, c client.Client, rc RunContext, token json.RawMessage) (bool, time.Duration, error) {
	var t ChaosMeshToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid chaosMesh token: %w", err)
	}
	obj := newChaosUnstructured(t.Kind, t.Namespace, t.Name)
	err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, obj)
	if errors.IsNotFound(err) {
		return true, 0, nil // never applied or already gone
	}
	if err != nil {
		return false, 0, err
	}
	if obj.GetLabels()[ScenarioLabel] != rc.ScenarioName {
		// A same-named object that is not ours: never touch it.
		return true, 0, nil
	}
	if obj.GetDeletionTimestamp() == nil {
		if err := c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !errors.IsNotFound(err) {
			return false, 0, err
		}
	}
	// Chaos Mesh objects carry their own finalizer that waits for the fault to
	// recover before the object disappears. Keep requeuing until it is gone; do
	// NOT strip finalizers.
	return false, 2 * time.Second, nil
}

// newChaosUnstructured returns an empty unstructured object with the apiVersion,
// kind, namespace and name set — enough for Get/Delete and as a Create base.
func newChaosUnstructured(kind, namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(chaosMeshAPIVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

// chaosObjectName resolves the chaos object's name: spec.Name or
// fs-<scenario>-<action>, sanitized to a DNS-1123 subdomain (<=63 chars).
func chaosObjectName(ca *v1alpha1.ChaosMeshAction, rc RunContext, actionName string) string {
	name := ca.Name
	if name == "" {
		name = fmt.Sprintf("fs-%s-%s", rc.ScenarioName, actionName)
	}
	return sanitizeDNS1123(name)
}

func chaosDuration(runDuration time.Duration) string {
	if runDuration <= 0 {
		return chaosUnboundedDuration
	}
	return (runDuration + chaosDeadmanMargin).String()
}

var dns1123Invalid = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeDNS1123 lowercases, replaces invalid chars with '-', trims leading and
// trailing '-', and caps the result at 63 characters.
func sanitizeDNS1123(s string) string {
	s = strings.ToLower(s)
	s = dns1123Invalid.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	if s == "" {
		s = "chaos"
	}
	return s
}
