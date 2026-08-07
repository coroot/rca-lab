package controller

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
	"github.com/coroot/rca-lab/operator/internal/actions"
)

// Sweeper runs once at manager start (leader only) and deletes objects
// labeled rcalab.dev/scenario whose owning scenario no longer exists or is
// idle with no recorded active actions. It is the belt-and-braces companion
// to ownerReference GC for state orphaned across operator restarts.
type Sweeper struct {
	Client    client.Client
	Namespace string
}

// NeedLeaderElection makes the sweeper run only on the elected leader, after
// caches have synced.
func (s *Sweeper) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It performs one sweep and returns.
func (s *Sweeper) Start(ctx context.Context) error {
	log := logf.Log.WithName("sweeper")
	log.Info("startup sweep: scanning for orphaned scenario objects", "namespace", s.Namespace)

	opts := []client.ListOption{client.InNamespace(s.Namespace), client.HasLabels{actions.ScenarioLabel}}

	jobs := &batchv1.JobList{}
	if err := s.Client.List(ctx, jobs, opts...); err != nil {
		return err
	}
	for i := range jobs.Items {
		s.sweep(ctx, &jobs.Items[i], "Job")
	}

	deps := &appsv1.DeploymentList{}
	if err := s.Client.List(ctx, deps, opts...); err != nil {
		return err
	}
	for i := range deps.Items {
		s.sweep(ctx, &deps.Items[i], "Deployment")
	}

	log.Info("startup sweep complete")
	return nil
}

func (s *Sweeper) sweep(ctx context.Context, obj client.Object, kind string) {
	log := logf.Log.WithName("sweeper")
	scenarioName := obj.GetLabels()[actions.ScenarioLabel]

	fs := &v1alpha1.FailureScenario{}
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: scenarioName}, fs)
	switch {
	case apierrors.IsNotFound(err):
		log.Info("ORPHANED OBJECT: owning FailureScenario does not exist, deleting",
			"kind", kind, "name", obj.GetName(), "scenario", scenarioName)
	case err != nil:
		log.Error(err, "failed to look up owning scenario, keeping object",
			"kind", kind, "name", obj.GetName(), "scenario", scenarioName)
		return
	default:
		idle := (fs.Status.Phase == "" || fs.Status.Phase == v1alpha1.PhaseIdle) &&
			len(fs.Status.ActiveActions) == 0 && fs.Status.CurrentRun == nil
		if !idle {
			return
		}
		log.Info("ORPHANED OBJECT: owning FailureScenario is idle with no active actions, deleting",
			"kind", kind, "name", obj.GetName(), "scenario", scenarioName)
	}

	if err := s.Client.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "failed to delete orphaned object", "kind", kind, "name", obj.GetName())
	}
}
