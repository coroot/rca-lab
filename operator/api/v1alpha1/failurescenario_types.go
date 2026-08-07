package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Scenario phases.
const (
	PhaseIdle       = "Idle"
	PhaseActivating = "Activating"
	PhaseActive     = "Active"
	PhaseReverting  = "Reverting"
	PhaseDegraded   = "Degraded"
)

// Run triggers.
const (
	RunTriggerManual  = "Manual"
	RunTriggerEnabled = "Enabled"
)

// Action phases (per active action).
const (
	ActionPhaseRecorded  = "Recorded"
	ActionPhaseApplied   = "Applied"
	ActionPhaseReverting = "Reverting"
)

// Run results (history).
const (
	RunResultCompleted = "Completed"
	RunResultReverted  = "Reverted"
	RunResultFailed    = "Failed"
)

// Condition types.
const (
	ConditionReady        = "Ready"
	ConditionRevertFailed = "RevertFailed"
)

// Action types.
const (
	ActionTypeWorkload    = "Workload"
	ActionTypeScale       = "Scale"
	ActionTypeDeployImage = "DeployImage"
)

// TriggerSpec requests a one-shot manual run: a runID different from
// status.lastCompletedRunID starts a run of the given duration.
type TriggerSpec struct {
	// RunID identifies the requested run. A NEW value (different from
	// status.lastCompletedRunID) starts a run.
	RunID string `json:"runID"`
	// Duration of the one-shot run. Falls back to spec.duration if empty.
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`
}

// WorkloadAction runs a database-load Job that looks like a real workload.
type WorkloadAction struct {
	// Name is the workload identity (Job name, app label, container name),
	// e.g. "analytics-reporting".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Image to run. Defaults to the operator's own image (env OPERATOR_IMAGE).
	// +optional
	Image string `json:"image,omitempty"`
	// Command overrides the generated dbtool arguments.
	// +optional
	Command []string `json:"command,omitempty"`
	// Engine selects the dbtool database driver.
	// +kubebuilder:validation:Enum=postgres;mysql
	// +optional
	Engine string `json:"engine,omitempty"`
	// Queries are SQL statements run in a loop by dbtool.
	// +optional
	Queries []string `json:"queries,omitempty"`
	// Concurrency is the number of parallel workers (Job parallelism).
	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	Concurrency int32 `json:"concurrency,omitempty"`
	// Interval is the pause between query-loop iterations per worker.
	// Defaults to 1s.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
	// Env is passed through to the workload container (DSN etc. via
	// secretKeyRef).
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
	// DeadlineMargin is added to the run duration to compute the Job's
	// activeDeadlineSeconds safety net. Defaults to 5m.
	// +optional
	DeadlineMargin *metav1.Duration `json:"deadlineMargin,omitempty"`
}

// ScaleTargetRef identifies the object a Scale action operates on.
type ScaleTargetRef struct {
	// +kubebuilder:validation:Enum=Deployment
	// +kubebuilder:default=Deployment
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ScaleAction scales a Deployment to a given replica count for the duration
// of the run.
type ScaleAction struct {
	TargetRef ScaleTargetRef `json:"targetRef"`
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
}

// DeployImageAction rolls out a different image for one container of a
// Deployment — a genuine version bump — and rolls it back on revert. The
// known-good image is recorded in three places so the rollback is always
// possible: the revert token in status, an annotation on the Deployment
// itself, and (declaratively) this spec.
type DeployImageAction struct {
	TargetRef ScaleTargetRef `json:"targetRef"`
	// Container is the container name within the Deployment's pod template.
	// +kubebuilder:validation:MinLength=1
	Container string `json:"container"`
	// Image to deploy (the regression variant, plausibly tagged).
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// KnownGoodImage is the declarative rollback target. If empty, the image
	// observed at activation time is used.
	// +optional
	KnownGoodImage string `json:"knownGoodImage,omitempty"`
	// RolloutTimeout bounds how long Ensure reports "progressing" before the
	// action counts as applied anyway (a crash-looping bad deploy is a valid
	// scenario, not an error). Defaults to 5m.
	// +optional
	RolloutTimeout *metav1.Duration `json:"rolloutTimeout,omitempty"`
}

// Action is a single failure-injection step.
// +kubebuilder:validation:XValidation:rule="(self.type == 'Workload') == has(self.workload) && (self.type == 'Scale') == has(self.scale) && (self.type == 'DeployImage') == has(self.deployImage)",message="exactly one of workload/scale/deployImage must be set and must match type"
type Action struct {
	// Name is unique within the scenario.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=Workload;Scale;DeployImage
	Type string `json:"type"`
	// Delay offsets this action's activation from the run start.
	// +optional
	Delay *metav1.Duration `json:"delay,omitempty"`
	// +optional
	Workload *WorkloadAction `json:"workload,omitempty"`
	// +optional
	Scale *ScaleAction `json:"scale,omitempty"`
	// +optional
	DeployImage *DeployImageAction `json:"deployImage,omitempty"`
}

// FailureScenarioSpec defines the desired state of a FailureScenario.
type FailureScenarioSpec struct {
	DisplayName string `json:"displayName"`
	// +optional
	Description string `json:"description,omitempty"`
	// +kubebuilder:validation:Enum=database;deploy;infra;app;network;kafka
	Category string `json:"category"`
	// +kubebuilder:validation:Enum=low;medium;high
	Severity string `json:"severity"`
	// ExpectedSymptoms documents the telemetry an RCA tool should observe
	// (used for grading, not by the operator).
	// +optional
	ExpectedSymptoms []string `json:"expectedSymptoms,omitempty"`
	// Enabled holds the failure active while true (sticky). If duration is
	// set, the run auto-expires and the operator flips enabled back to false.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// Trigger starts a one-shot manual run.
	// +optional
	Trigger *TriggerSpec `json:"trigger,omitempty"`
	// Duration is the default hold time for enabled=true runs (auto-expiry).
	// Empty means hold until disabled.
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Actions []Action `json:"actions"`
}

// CurrentRun describes the run in progress.
type CurrentRun struct {
	ID string `json:"id"`
	// +kubebuilder:validation:Enum=Manual;Enabled
	Trigger   string      `json:"trigger"`
	StartedAt metav1.Time `json:"startedAt"`
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// ActiveAction is the durable record of one injected (or in-flight) action.
// The revert token is fully self-contained: reverting never consults spec.
type ActiveAction struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=Recorded;Applied;Reverting
	Phase string `json:"phase"`
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
	// +optional
	Attempts int32 `json:"attempts,omitempty"`
	// +optional
	LastError string `json:"lastError,omitempty"`
	// Revert is the opaque, self-contained revert token persisted BEFORE any
	// mutation is made.
	Revert apiextensionsv1.JSON `json:"revert"`
}

// RunHistoryEntry records a finished run.
type RunHistoryEntry struct {
	ID        string      `json:"id"`
	Trigger   string      `json:"trigger"`
	StartedAt metav1.Time `json:"startedAt"`
	// +optional
	EndedAt *metav1.Time `json:"endedAt,omitempty"`
	// +kubebuilder:validation:Enum=Completed;Reverted;Failed
	Result string `json:"result"`
	// +optional
	Message string `json:"message,omitempty"`
}

// FailureScenarioStatus defines the observed state of a FailureScenario.
type FailureScenarioStatus struct {
	// +kubebuilder:validation:Enum=Idle;Activating;Active;Reverting;Degraded
	// +kubebuilder:default=Idle
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	CurrentRun *CurrentRun `json:"currentRun,omitempty"`
	// +optional
	ActiveActions []ActiveAction `json:"activeActions,omitempty"`
	// +optional
	LastCompletedRunID string `json:"lastCompletedRunID,omitempty"`
	// +kubebuilder:validation:MaxItems=10
	// +optional
	History []RunHistoryEntry `json:"history,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fs
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Category",type=string,JSONPath=`.spec.category`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.spec.severity`
// +kubebuilder:printcolumn:name="Enabled",type=boolean,JSONPath=`.spec.enabled`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FailureScenario is a reproducible failure that the operator can inject and
// durably revert.
type FailureScenario struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec FailureScenarioSpec `json:"spec"`
	// +optional
	Status FailureScenarioStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FailureScenarioList contains a list of FailureScenario.
type FailureScenarioList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FailureScenario `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FailureScenario{}, &FailureScenarioList{})
}
