package actions

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testRC() RunContext {
	return RunContext{
		ScenarioName:  "pg-analytics-queries",
		ScenarioUID:   types.UID("uid-1"),
		Namespace:     "default",
		RunID:         "run-1",
		OperatorImage: "ghcr.io/coroot/rca-lab/operator:1.0.0",
	}
}

// TestWorkloadPlanTokenRoundTrip checks that the Workload Plan token is
// self-contained and round-trips, and that a name conflict is rejected.
func TestWorkloadPlanTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	rc := testRC()
	spec := &v1alpha1.Action{
		Name: "analytics-reporting",
		Type: v1alpha1.ActionTypeWorkload,
		Workload: &v1alpha1.WorkloadAction{
			Name:    "analytics-reporting",
			Engine:  "postgres",
			Queries: []string{"SELECT 1"},
		},
	}
	w := &Workload{}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	raw, err := w.Plan(ctx, c, rc, spec)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var tok WorkloadToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("token does not round-trip: %v", err)
	}
	want := WorkloadToken{Kind: "Job", Namespace: "default", Name: "analytics-reporting"}
	if tok != want {
		t.Fatalf("token = %+v, want %+v", tok, want)
	}

	// A foreign Job with the same name must fail the plan.
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "analytics-reporting", Namespace: "default",
		Labels: map[string]string{ScenarioLabel: "someone-else"},
	}}
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(foreign).Build()
	if _, err := w.Plan(ctx, c, rc, spec); err == nil {
		t.Fatal("Plan should reject a name conflict with a foreign job")
	}
}

// TestScaleTokenRoundTrip checks that Plan captures the live replica count in
// a self-contained token and that Revert restores it from the token alone.
func TestScaleTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	rc := testRC()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "load-generator", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dep).Build()
	spec := &v1alpha1.Action{
		Name: "scale-up-load",
		Type: v1alpha1.ActionTypeScale,
		Scale: &v1alpha1.ScaleAction{
			TargetRef: v1alpha1.ScaleTargetRef{Kind: "Deployment", Name: "load-generator"},
			Replicas:  5,
		},
	}
	s := &Scale{}

	raw, err := s.Plan(ctx, c, rc, spec)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var tok ScaleToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		t.Fatalf("token does not round-trip: %v", err)
	}
	want := ScaleToken{Kind: "Deployment", Namespace: "default", Name: "load-generator", PreviousReplicas: 2}
	if tok != want {
		t.Fatalf("token = %+v, want %+v", tok, want)
	}

	done, _, err := s.Ensure(ctx, c, rc, spec, raw)
	if err != nil || !done {
		t.Fatalf("Ensure: done=%v err=%v", done, err)
	}
	got := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "load-generator"}, got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 5 || got.Annotations[HeldByAnnotation] != "pg-analytics-queries/run-1" {
		t.Fatalf("Ensure did not apply: replicas=%d annotations=%v", *got.Spec.Replicas, got.Annotations)
	}

	// Revert from the token alone (twice: must be idempotent).
	for i := 0; i < 2; i++ {
		done, _, err = s.Revert(ctx, c, rc, raw)
		if err != nil || !done {
			t.Fatalf("Revert #%d: done=%v err=%v", i+1, done, err)
		}
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "load-generator"}, got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 2 {
		t.Fatalf("Revert did not restore replicas: got %d, want 2", *got.Spec.Replicas)
	}
	if _, held := got.Annotations[HeldByAnnotation]; held {
		t.Fatalf("Revert did not remove the held-by annotation: %v", got.Annotations)
	}

	// Revert with a vanished target is done, not an error.
	empty := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	done, _, err = s.Revert(ctx, empty, rc, raw)
	if err != nil || !done {
		t.Fatalf("Revert on vanished target: done=%v err=%v", done, err)
	}
}
