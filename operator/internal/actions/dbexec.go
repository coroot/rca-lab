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
	Register(&DBExec{})
}

const (
	// execJobDeadline bounds a single one-off exec Job. Generous because an
	// Ensure statement can be a large data change; Revert statements are
	// expected to be quick (re-enable a setting, ANALYZE, delete a marked set).
	execJobDeadline = 20 * time.Minute
	// execRevertSuffix names the revert Job relative to the action name.
	execRevertSuffix = "-revert"
)

// DBExecToken is the self-contained revert token of a DBExec action. It carries
// everything Revert needs — the revert statements never come from the spec.
type DBExecToken struct {
	Namespace  string          `json:"namespace"`
	Engine     string          `json:"engine"`
	EnsureName string          `json:"ensureName"`
	RevertName string          `json:"revertName"`
	Env        []corev1.EnvVar `json:"env,omitempty"`
	Revert     []string        `json:"revert,omitempty"`
}

// DBExec runs one-off SQL as short-lived Jobs: the Ensure statements once on
// activation and the Revert statements once on revert.
type DBExec struct{}

func (d *DBExec) Type() string { return v1alpha1.ActionTypeDBExec }

func (d *DBExec) Plan(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action) (json.RawMessage, error) {
	da := spec.DBExec
	if da == nil {
		return nil, fmt.Errorf("action %q: dbExec config missing", spec.Name)
	}
	ensureName := da.Name
	revertName := da.Name + execRevertSuffix
	// Refuse to adopt a same-named Job that belongs to something else.
	for _, name := range []string{ensureName, revertName} {
		existing := &batchv1.Job{}
		err := c.Get(ctx, types.NamespacedName{Namespace: rc.Namespace, Name: name}, existing)
		switch {
		case err == nil:
			if existing.Labels[ScenarioLabel] != rc.ScenarioName {
				return nil, fmt.Errorf("job %s/%s already exists and is not owned by scenario %q (name conflict)",
					rc.Namespace, name, rc.ScenarioName)
			}
		case !errors.IsNotFound(err):
			return nil, err
		}
	}
	return json.Marshal(DBExecToken{
		Namespace:  rc.Namespace,
		Engine:     da.Engine,
		EnsureName: ensureName,
		RevertName: revertName,
		Env:        da.Env,
		Revert:     da.Revert,
	})
}

func (d *DBExec) Ensure(ctx context.Context, c client.Client, rc RunContext, spec *v1alpha1.Action, token json.RawMessage) (bool, time.Duration, error) {
	da := spec.DBExec
	if da == nil {
		return false, 0, fmt.Errorf("action %q: dbExec config missing", spec.Name)
	}
	var t DBExecToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid dbexec token: %w", err)
	}
	return d.runJob(ctx, c, rc, t.EnsureName, da.Engine, da.Env, da.Ensure)
}

func (d *DBExec) Revert(ctx context.Context, c client.Client, rc RunContext, token json.RawMessage) (bool, time.Duration, error) {
	var t DBExecToken
	if err := json.Unmarshal(token, &t); err != nil {
		return false, 0, fmt.Errorf("invalid dbexec token: %w", err)
	}
	if len(t.Revert) == 0 {
		return true, 0, nil // nothing to undo
	}
	return d.runJob(ctx, c, rc, t.RevertName, t.Engine, t.Env, t.Revert)
}

// runJob drives a single one-off exec Job to completion, idempotently: it
// creates the Job if absent, reports done once it Succeeds, and returns an
// error if it Fails (so the controller surfaces a real SQL/config problem
// instead of hammering). Safe to call repeatedly and across restarts.
func (d *DBExec) runJob(ctx context.Context, c client.Client, rc RunContext, name, engine string, env []corev1.EnvVar, queries []string) (bool, time.Duration, error) {
	job := &batchv1.Job{}
	err := c.Get(ctx, types.NamespacedName{Namespace: rc.Namespace, Name: name}, job)
	switch {
	case err == nil:
		if job.Labels[ScenarioLabel] != rc.ScenarioName {
			return false, 0, fmt.Errorf("job %s/%s exists but is not ours", rc.Namespace, name)
		}
		if job.Status.Succeeded > 0 {
			return true, 0, nil
		}
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				// A Job is immutable, so a failed one can't retry in place.
				// Delete it (best effort) and surface the error; the controller
				// paces the retry with backoff and recreates on the next pass.
				if job.DeletionTimestamp == nil {
					_ = c.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
				}
				return false, 0, fmt.Errorf("exec job %s failed: %s", name, cond.Message)
			}
		}
		return false, 2 * time.Second, nil // still running
	case !errors.IsNotFound(err):
		return false, 0, err
	}

	if err := c.Create(ctx, d.buildJob(rc, name, engine, env, queries)); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, time.Second, nil // re-observe
		}
		return false, 0, err
	}
	return false, 2 * time.Second, nil
}

func (d *DBExec) buildJob(rc RunContext, name, engine string, env []corev1.EnvVar, queries []string) *batchv1.Job {
	args := []string{"dbtool", "--engine", engine, "--once"}
	for _, q := range queries {
		args = append(args, "--query", q)
	}
	labels := map[string]string{
		ScenarioLabel: rc.ScenarioName,
		"app":         name,
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
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
			Parallelism:             ptr.To(int32(1)),
			Completions:             ptr.To(int32(1)),
			BackoffLimit:            ptr.To(int32(4)),
			ActiveDeadlineSeconds:   ptr.To(int64(execJobDeadline / time.Second)),
			TTLSecondsAfterFinished: ptr.To(int32(600)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  name,
						Image: rc.OperatorImage,
						Args:  args,
						Env:   env,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
	}
}
