package controller

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

// TestAppendHistoryCap checks the history is capped at the most recent 10
// entries.
func TestAppendHistoryCap(t *testing.T) {
	var h []v1alpha1.RunHistoryEntry
	for i := 0; i < 25; i++ {
		h = appendHistory(h, v1alpha1.RunHistoryEntry{
			ID:        fmt.Sprintf("run-%d", i),
			Trigger:   v1alpha1.RunTriggerManual,
			StartedAt: metav1.Now(),
			Result:    v1alpha1.RunResultCompleted,
		})
	}
	if len(h) != historyCap {
		t.Fatalf("history length = %d, want %d", len(h), historyCap)
	}
	if h[0].ID != "run-15" || h[len(h)-1].ID != "run-24" {
		t.Fatalf("history should keep the most recent entries, got first=%s last=%s", h[0].ID, h[len(h)-1].ID)
	}
}

// TestDesiredRunAndStillWanted covers the phase-machine helpers deciding when
// a run starts and when it should keep running.
func TestDesiredRunAndStillWanted(t *testing.T) {
	fs := &v1alpha1.FailureScenario{}

	// Nothing requested.
	if trig, _, _ := desiredRun(fs); trig != "" {
		t.Fatalf("desiredRun on empty spec = %q, want none", trig)
	}

	// enabled=true starts an Enabled run with spec.duration.
	fs.Spec.Enabled = true
	fs.Spec.Duration = &metav1.Duration{Duration: 20 * time.Minute}
	trig, runID, dur := desiredRun(fs)
	if trig != v1alpha1.RunTriggerEnabled || runID == "" || dur != 20*time.Minute {
		t.Fatalf("desiredRun(enabled) = (%q, %q, %s)", trig, runID, dur)
	}

	// Manual trigger with a NEW runID starts a Manual run using its duration.
	fs.Spec.Enabled = false
	fs.Spec.Trigger = &v1alpha1.TriggerSpec{RunID: "run-42", Duration: &metav1.Duration{Duration: 5 * time.Minute}}
	trig, runID, dur = desiredRun(fs)
	if trig != v1alpha1.RunTriggerManual || runID != "run-42" || dur != 5*time.Minute {
		t.Fatalf("desiredRun(trigger) = (%q, %q, %s)", trig, runID, dur)
	}

	// The same runID does not restart once completed.
	fs.Status.LastCompletedRunID = "run-42"
	if trig, _, _ = desiredRun(fs); trig != "" {
		t.Fatalf("desiredRun(completed trigger) = %q, want none", trig)
	}

	// Still-wanted logic.
	run := &v1alpha1.CurrentRun{ID: "run-42", Trigger: v1alpha1.RunTriggerManual}
	if !runStillWanted(fs, run) {
		t.Fatal("manual run with matching trigger.runID should still be wanted")
	}
	fs.Spec.Trigger.RunID = "run-43"
	if runStillWanted(fs, run) {
		t.Fatal("manual run should stop once trigger.runID changes")
	}
	run = &v1alpha1.CurrentRun{ID: "x", Trigger: v1alpha1.RunTriggerEnabled}
	fs.Spec.Enabled = true
	if !runStillWanted(fs, run) {
		t.Fatal("enabled run should be wanted while enabled=true")
	}
	fs.Spec.Enabled = false
	if runStillWanted(fs, run) {
		t.Fatal("enabled run should stop once enabled=false")
	}
}

// TestBackoffFor sanity-checks the retry backoff shape.
func TestBackoffFor(t *testing.T) {
	if backoffFor(1) != time.Second {
		t.Fatalf("backoffFor(1) = %s", backoffFor(1))
	}
	if backoffFor(10) != 16*time.Second {
		t.Fatalf("backoffFor(10) = %s, want capped 16s", backoffFor(10))
	}
}
