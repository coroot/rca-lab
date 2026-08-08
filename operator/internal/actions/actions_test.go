package actions

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

// TestChaosMeshTokenAndBuild checks the ChaosMesh token round-trips and that
// buildChaos wires the dead-man duration, owner ref, scenario label and
// verbatim spec passthrough.
func TestChaosMeshTokenAndBuild(t *testing.T) {
	rc := testRC()
	rc.RunDuration = 15 * time.Minute

	ca := &v1alpha1.ChaosMeshAction{
		Kind: "NetworkChaos",
		Spec: apiextensionsv1.JSON{Raw: []byte(`{"action":"delay","mode":"all"}`)},
	}
	spec := &v1alpha1.Action{Name: "net-delay", Type: v1alpha1.ActionTypeChaosMesh, ChaosMesh: ca}
	m := &ChaosMesh{}

	name := chaosObjectName(ca, rc, spec.Name)
	if name != "fs-pg-analytics-queries-net-delay" {
		t.Fatalf("name = %q", name)
	}

	tok := ChaosMeshToken{APIVersion: chaosMeshAPIVersion, Kind: ca.Kind, Namespace: rc.Namespace, Name: name}
	raw, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	var back ChaosMeshToken
	if err := json.Unmarshal(raw, &back); err != nil || back != tok {
		t.Fatalf("token round-trip: %+v err=%v", back, err)
	}

	obj, err := m.buildChaos(rc, ca, tok)
	if err != nil {
		t.Fatalf("buildChaos: %v", err)
	}
	if obj.GetAPIVersion() != chaosMeshAPIVersion || obj.GetKind() != "NetworkChaos" {
		t.Fatalf("gvk = %s/%s", obj.GetAPIVersion(), obj.GetKind())
	}
	if obj.GetLabels()[ScenarioLabel] != rc.ScenarioName {
		t.Fatalf("missing scenario label: %v", obj.GetLabels())
	}
	if refs := obj.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != rc.ScenarioName || refs[0].UID != rc.ScenarioUID {
		t.Fatalf("owner refs = %+v", refs)
	}
	cs, _ := obj.Object["spec"].(map[string]any)
	if cs["action"] != "delay" || cs["mode"] != "all" {
		t.Fatalf("spec not passed through: %v", cs)
	}
	// Dead-man switch: run duration + 5m margin.
	if cs["duration"] != "20m0s" {
		t.Fatalf("duration = %v, want 20m0s", cs["duration"])
	}

	// A caller-provided duration is left untouched; 0 run duration -> 24h.
	ca2 := &v1alpha1.ChaosMeshAction{Kind: "StressChaos", Spec: apiextensionsv1.JSON{Raw: []byte(`{"duration":"3m"}`)}}
	obj2, _ := m.buildChaos(rc, ca2, tok)
	if obj2.Object["spec"].(map[string]any)["duration"] != "3m" {
		t.Fatalf("caller duration overwritten")
	}
	rc.RunDuration = 0
	obj3, _ := m.buildChaos(rc, ca, tok)
	if obj3.Object["spec"].(map[string]any)["duration"] != "24h" {
		t.Fatalf("unbounded duration = %v, want 24h", obj3.Object["spec"].(map[string]any)["duration"])
	}
}
