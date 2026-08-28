package api

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

func TestSummarizeDefaultsPhaseAndSymptoms(t *testing.T) {
	fs := &v1alpha1.FailureScenario{
		ObjectMeta: metav1.ObjectMeta{Name: "db-slow"},
		Spec: v1alpha1.FailureScenarioSpec{
			DisplayName: "Slow DB",
			Category:    "database",
			Icon:        "mysql",
			Enabled:     true,
		},
	}

	got := summarize(fs)

	if got.Name != "db-slow" || got.DisplayName != "Slow DB" {
		t.Fatalf("unexpected identity: %+v", got)
	}
	if got.Phase != v1alpha1.PhaseIdle {
		t.Errorf("empty status phase should default to %q, got %q", v1alpha1.PhaseIdle, got.Phase)
	}
	if got.ExpectedSymptoms == nil {
		t.Errorf("expectedSymptoms should be non-nil (renders as [] in JSON)")
	}
	if !got.Enabled {
		t.Errorf("enabled should be true")
	}
}
