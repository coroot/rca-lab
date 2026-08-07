package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

func init() {
	Register(&Workload{})
}

const (
	defaultConcurrency    = int32(2)
	defaultInterval       = time.Second
	defaultDeadlineMargin = 5 * time.Minute
	// unboundedRunDeadline caps Jobs of scenarios without duration/expiry.
	unboundedRunDeadline = 24 * time.Hour
)

// WorkloadToken is the self-contained revert token of a Workload action.
type WorkloadToken struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Workload runs a realistic database-load actor as a batch/v1 Job.
type Workload struct{}

func (w *Workload) Type() string { return v1alpha1.ActionTypeWorkload }

func (w *Workload) Plan(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action) (json.RawMessage, error) {
	wa := spec.Workload
	if wa == nil {
		return nil, fmt.Errorf("action %q: workload config missing", spec.Name)
	}
	existing := &batchv1.Job{}
	err := c.Get(ctx, types.NamespacedName{Namespace: rc.Namespace, Name: wa.Name}, existing)
	switch {
	case err == nil:
		if existing.Labels[ScenarioLabel] != rc.ScenarioName {
			return nil, fmt.Errorf("job %s/%s already exists and is not owned by scenario %q (name conflict)",
				rc.Namespace, wa.Name, rc.ScenarioName)
		}
	case !errors.IsNotFound(err):
		return nil, err
	}
	return json.Marshal(WorkloadToken{Kind: "Job", Namespace: rc.Namespace, Name: wa.Name})
}

func (w *Workload) Ensure(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action, token json.RawMessage) (bool, time.Duration, error) {
	var t WorkloadToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid workload token: %w", err)
	}
	existing := &batchv1.Job{}
	err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, existing)
	switch {
	case err == nil:
		if existing.Labels[ScenarioLabel] != rc.ScenarioName {
			return false, 0, fmt.Errorf("job %s/%s exists but is not ours", t.Namespace, t.Name)
		}
		return true, 0, nil // exists and ours: done (don't block on pod readiness)
	case !errors.IsNotFound(err):
		return false, 0, err
	}

	job := w.buildJob(rc, spec)
	if err := c.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, time.Second, nil // re-observe on next pass
		}
		return false, 0, err
	}
	return true, 0, nil
}

func (w *Workload) buildJob(rc RunContext, spec *v1alpha1.Action) *batchv1.Job {
	wa := spec.Workload

	concurrency := wa.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	interval := defaultInterval
	if wa.Interval != nil {
		interval = wa.Interval.Duration
	}
	margin := defaultDeadlineMargin
	if wa.DeadlineMargin != nil {
		margin = wa.DeadlineMargin.Duration
	}
	deadline := unboundedRunDeadline
	if rc.RunDuration > 0 {
		deadline = rc.RunDuration + margin
	}

	image := wa.Image
	if image == "" {
		image = rc.OperatorImage
	}
	args := wa.Command
	if len(args) == 0 {
		// Concurrency is expressed via Job parallelism; each pod runs one loop.
		args = []string{"dbtool", "--engine", wa.Engine, "--interval", interval.String(), "--concurrency", "1"}
		for _, q := range wa.Queries {
			args = append(args, "--query", q)
		}
	}

	labels := map[string]string{
		ScenarioLabel: rc.ScenarioName,
		"app":         wa.Name,
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wa.Name,
			Namespace: rc.Namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1alpha1.GroupVersion.String(),
				Kind:               "FailureScenario",
				Name:               rc.ScenarioName,
				UID:                rc.ScenarioUID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(false),
			}},
		},
		Spec: batchv1.JobSpec{
			Parallelism:             ptr.To(concurrency),
			Completions:             nil, // run until deadline
			BackoffLimit:            ptr.To(int32(100)),
			ActiveDeadlineSeconds:   ptr.To(int64(deadline / time.Second)),
			TTLSecondsAfterFinished: ptr.To(int32(600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  wa.Name,
						Image: image,
						Args:  args,
						Env:   wa.Env,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					}},
				},
			},
		},
	}
}

func (w *Workload) Revert(ctx context.Context, c client.Client, rc RunContext, token json.RawMessage) (bool, time.Duration, error) {
	var t WorkloadToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid workload token: %w", err)
	}
	job := &batchv1.Job{}
	err := c.Get(ctx, types.NamespacedName{Namespace: t.Namespace, Name: t.Name}, job)
	if errors.IsNotFound(err) {
		return true, 0, nil // never applied or already gone
	}
	if err != nil {
		return false, 0, err
	}
	if job.Labels[ScenarioLabel] != rc.ScenarioName {
		// A same-named Job that is not ours (e.g. recreated after our delete):
		// never touch it.
		return true, 0, nil
	}
	if job.DeletionTimestamp == nil {
		if err := c.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !errors.IsNotFound(err) {
			return false, 0, err
		}
	}
	return false, 2 * time.Second, nil // wait until gone
}
