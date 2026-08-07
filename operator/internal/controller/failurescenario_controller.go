// Package controller reconciles FailureScenario resources.
//
// The reconciler is strictly level-triggered: it never blocks, and every
// mutation of the world is preceded by persisting a self-contained revert
// token to the scenario status (plan -> persist token -> mutate). Reverts are
// driven from those tokens alone, so they survive operator restarts.
package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
	"github.com/coroot/rca-lab/operator/internal/actions"
)

const (
	// FinalizerName guarantees reverts run before a scenario disappears.
	FinalizerName = "rcalab.dev/revert"
	// ForceReleaseAnnotation skips draining on deletion; remaining tokens are
	// dumped into the leaked-state ConfigMap instead.
	ForceReleaseAnnotation = "rcalab.dev/force-release"
	// LeakedStateConfigMap collects tokens abandoned via force-release.
	LeakedStateConfigMap = "rca-lab-leaked-state"

	historyCap            = 10
	steadyStateResync     = 30 * time.Second
	degradedAfterFailures = 5
)

// FailureScenarioReconciler reconciles FailureScenario objects.
type FailureScenarioReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	OperatorImage string
	Namespace     string
}

// +kubebuilder:rbac:groups=rcalab.dev,resources=failurescenarios;failurescenarios/status;failurescenarios/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func (r *FailureScenarioReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	fs := &v1alpha1.FailureScenario{}
	if err := r.Get(ctx, req.NamespacedName, fs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !fs.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, fs)
	}

	if !controllerutil.ContainsFinalizer(fs, FinalizerName) {
		controllerutil.AddFinalizer(fs, FinalizerName)
		if err := r.Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := validateSpec(&fs.Spec); err != nil {
		log.Info("invalid spec", "error", err)
		perr := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
			setCondition(s, fs.Generation, v1alpha1.ConditionReady, metav1.ConditionFalse, "InvalidSpec", err.Error())
		})
		return ctrl.Result{}, perr // nothing to do until the spec changes
	}

	run := fs.Status.CurrentRun
	if run == nil {
		if len(fs.Status.ActiveActions) > 0 {
			// Crash leftovers from an earlier interrupted run: drain them.
			return r.reconcileRevert(ctx, fs, nil, false)
		}
		return r.maybeStartRun(ctx, fs)
	}

	now := time.Now()
	expired := run.ExpiresAt != nil && !now.Before(run.ExpiresAt.Time)
	if expired && run.Trigger == v1alpha1.RunTriggerEnabled && fs.Spec.Enabled {
		// Auto-expiry of an enabled run: flip enabled off so the run does not
		// immediately restart after the revert.
		fs.Spec.Enabled = false
		if err := r.Update(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("run expired, disabling scenario", "runID", run.ID)
	}

	if expired || !runStillWanted(fs, run) {
		return r.reconcileRevert(ctx, fs, run, false)
	}
	return r.reconcileActivate(ctx, fs, run)
}

// maybeStartRun starts a new run if the spec asks for one, otherwise settles
// into Idle.
func (r *FailureScenarioReconciler) maybeStartRun(ctx context.Context, fs *v1alpha1.FailureScenario) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	trigger, runID, duration := desiredRun(fs)
	if trigger == "" {
		if fs.Status.Phase != v1alpha1.PhaseIdle || !meta.IsStatusConditionTrue(fs.Status.Conditions, v1alpha1.ConditionReady) {
			err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
				s.Phase = v1alpha1.PhaseIdle
				setCondition(s, fs.Generation, v1alpha1.ConditionReady, metav1.ConditionTrue, "Idle", "scenario is idle")
			})
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	now := metav1.Now()
	cr := &v1alpha1.CurrentRun{ID: runID, Trigger: trigger, StartedAt: now}
	if duration > 0 {
		t := metav1.NewTime(now.Add(duration))
		cr.ExpiresAt = &t
	}
	if err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
		s.CurrentRun = cr
		s.Phase = v1alpha1.PhaseActivating
	}); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("starting run", "runID", runID, "trigger", trigger, "duration", duration.String())
	r.Recorder.Eventf(fs, corev1.EventTypeNormal, "RunStarted", "run %s started (trigger=%s, duration=%s)", runID, trigger, duration)
	return ctrl.Result{Requeue: true}, nil
}

// reconcileActivate drives the scenario toward (and holds it in) the injected
// state. Invariant: each action's revert token is persisted to status before
// the action mutates anything.
func (r *FailureScenarioReconciler) reconcileActivate(ctx context.Context, fs *v1alpha1.FailureScenario, run *v1alpha1.CurrentRun) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := time.Now()
	rc := r.runContext(fs, run)

	var requeueAfter time.Duration // 0 = none yet
	allApplied := true

	for i := range fs.Spec.Actions {
		act := &fs.Spec.Actions[i]
		impl := actions.Get(act.Type)
		if impl == nil {
			return ctrl.Result{}, fmt.Errorf("unknown action type %q", act.Type)
		}

		if findActiveAction(fs.Status.ActiveActions, act.Name) < 0 {
			allApplied = false
			activateAt := run.StartedAt.Time
			if act.Delay != nil {
				activateAt = activateAt.Add(act.Delay.Duration)
			}
			if now.Before(activateAt) {
				lowerRequeue(&requeueAfter, activateAt.Sub(now))
				continue
			}
			// 1. Plan (read-only).
			token, err := impl.Plan(ctx, r.Client, rc, act)
			if err != nil {
				log.Error(err, "plan failed", "action", act.Name)
				r.Recorder.Eventf(fs, corev1.EventTypeWarning, "PlanFailed", "action %s: %v", act.Name, err)
				if perr := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
					setCondition(s, fs.Generation, v1alpha1.ConditionReady, metav1.ConditionFalse, "PlanFailed",
						fmt.Sprintf("action %s: %v", act.Name, err))
				}); perr != nil {
					return ctrl.Result{}, perr
				}
				lowerRequeue(&requeueAfter, 10*time.Second)
				continue
			}
			// 2. Persist the revert token BEFORE mutating anything.
			aa := v1alpha1.ActiveAction{
				Name:   act.Name,
				Type:   act.Type,
				Phase:  v1alpha1.ActionPhaseRecorded,
				Revert: apiJSON(token),
			}
			if err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
				if findActiveAction(s.ActiveActions, act.Name) < 0 {
					s.ActiveActions = append(s.ActiveActions, aa)
				}
				s.Phase = v1alpha1.PhaseActivating
			}); err != nil {
				return ctrl.Result{}, err
			}
		}

		idx := findActiveAction(fs.Status.ActiveActions, act.Name)
		aa := fs.Status.ActiveActions[idx]

		// 3. Mutate (converge), driven by the persisted token.
		done, ra, err := impl.Ensure(ctx, r.Client, rc, act, aa.Revert.Raw)
		if err != nil {
			log.Error(err, "ensure failed", "action", act.Name, "attempts", aa.Attempts+1)
			if aa.Phase != v1alpha1.ActionPhaseApplied {
				allApplied = false
			}
			if perr := r.patchStatus(ctx, fs, statusMutation(act.Name, func(a *v1alpha1.ActiveAction) {
				a.Attempts++
				a.LastError = err.Error()
			})); perr != nil {
				return ctrl.Result{}, perr
			}
			lowerRequeue(&requeueAfter, backoffFor(aa.Attempts+1))
			continue
		}
		if done {
			if aa.Phase != v1alpha1.ActionPhaseApplied || aa.LastError != "" {
				appliedAt := metav1.NewTime(now)
				if err := r.patchStatus(ctx, fs, statusMutation(act.Name, func(a *v1alpha1.ActiveAction) {
					if a.Phase != v1alpha1.ActionPhaseApplied {
						a.Phase = v1alpha1.ActionPhaseApplied
						a.AppliedAt = &appliedAt
					}
					a.Attempts = 0
					a.LastError = ""
				})); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else {
			allApplied = false
			if ra <= 0 {
				ra = 5 * time.Second
			}
			lowerRequeue(&requeueAfter, ra)
		}
	}

	phase := v1alpha1.PhaseActivating
	if allApplied {
		phase = v1alpha1.PhaseActive
	}
	if fs.Status.Phase != phase || !meta.IsStatusConditionTrue(fs.Status.Conditions, v1alpha1.ConditionReady) {
		if err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
			s.Phase = phase
			if allApplied {
				setCondition(s, fs.Generation, v1alpha1.ConditionReady, metav1.ConditionTrue, "Active", "all actions applied")
			}
		}); err != nil {
			return ctrl.Result{}, err
		}
		if phase == v1alpha1.PhaseActive && fs.Status.Phase == v1alpha1.PhaseActive {
			r.Recorder.Eventf(fs, corev1.EventTypeNormal, "Active", "run %s: all actions applied", run.ID)
		}
	}

	// Steady state: re-Ensure every 30s and wake up exactly at expiry.
	lowerRequeue(&requeueAfter, steadyStateResync)
	if run.ExpiresAt != nil {
		until := time.Until(run.ExpiresAt.Time)
		if until < time.Second {
			until = time.Second
		}
		lowerRequeue(&requeueAfter, until)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// reconcileRevert drains activeActions in reverse order, persisting after
// every step. run may be nil (crash leftovers); deleting selects
// finalizer-removal instead of run bookkeeping once drained.
func (r *FailureScenarioReconciler) reconcileRevert(ctx context.Context, fs *v1alpha1.FailureScenario, run *v1alpha1.CurrentRun, deleting bool) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if len(fs.Status.ActiveActions) == 0 {
		return r.finishRun(ctx, fs, run, deleting, v1alpha1.RunResultReverted, "")
	}

	if fs.Status.Phase != v1alpha1.PhaseReverting && fs.Status.Phase != v1alpha1.PhaseDegraded {
		if err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
			s.Phase = v1alpha1.PhaseReverting
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	rc := r.runContext(fs, run)
	aa := fs.Status.ActiveActions[len(fs.Status.ActiveActions)-1]
	impl := actions.Get(aa.Type)
	if impl == nil {
		return ctrl.Result{}, fmt.Errorf("unknown action type %q in active state", aa.Type)
	}

	done, ra, err := impl.Revert(ctx, r.Client, rc, aa.Revert.Raw)
	if err != nil {
		attempts := aa.Attempts + 1
		log.Error(err, "revert failed", "action", aa.Name, "attempts", attempts)
		r.Recorder.Eventf(fs, corev1.EventTypeWarning, "RevertFailed", "action %s: %v", aa.Name, err)
		degraded := attempts >= degradedAfterFailures
		if perr := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
			if i := findActiveAction(s.ActiveActions, aa.Name); i >= 0 {
				s.ActiveActions[i].Phase = v1alpha1.ActionPhaseReverting
				s.ActiveActions[i].Attempts = attempts
				s.ActiveActions[i].LastError = err.Error()
			}
			if degraded {
				s.Phase = v1alpha1.PhaseDegraded
				setCondition(s, fs.Generation, v1alpha1.ConditionRevertFailed, metav1.ConditionTrue, "RevertFailing",
					fmt.Sprintf("action %s failed to revert %d times: %v", aa.Name, attempts, err))
			}
		}); perr != nil {
			return ctrl.Result{}, perr
		}
		// KEEP trying, with backoff.
		return ctrl.Result{RequeueAfter: backoffFor(attempts)}, nil
	}

	if !done {
		if ra <= 0 {
			ra = 2 * time.Second
		}
		if err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
			if i := findActiveAction(s.ActiveActions, aa.Name); i >= 0 {
				s.ActiveActions[i].Phase = v1alpha1.ActionPhaseReverting
				s.ActiveActions[i].LastError = ""
			}
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: ra}, nil
	}

	// Drained one action: persist its removal, then continue immediately.
	log.Info("action reverted", "action", aa.Name)
	if err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
		if i := findActiveAction(s.ActiveActions, aa.Name); i >= 0 {
			s.ActiveActions = append(s.ActiveActions[:i], s.ActiveActions[i+1:]...)
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// finishRun closes the books on a drained run.
func (r *FailureScenarioReconciler) finishRun(ctx context.Context, fs *v1alpha1.FailureScenario, run *v1alpha1.CurrentRun, deleting bool, result, message string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if deleting {
		controllerutil.RemoveFinalizer(fs, FinalizerName)
		return ctrl.Result{}, r.Update(ctx, fs)
	}

	if run != nil {
		now := metav1.Now()
		if run.ExpiresAt != nil && !now.Time.Before(run.ExpiresAt.Time) {
			result = v1alpha1.RunResultCompleted
		}
		entry := v1alpha1.RunHistoryEntry{
			ID:        run.ID,
			Trigger:   run.Trigger,
			StartedAt: run.StartedAt,
			EndedAt:   &now,
			Result:    result,
			Message:   message,
		}
		if err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
			s.History = appendHistory(s.History, entry)
			s.LastCompletedRunID = run.ID
			s.CurrentRun = nil
			s.Phase = v1alpha1.PhaseIdle
			meta.RemoveStatusCondition(&s.Conditions, v1alpha1.ConditionRevertFailed)
			setCondition(s, fs.Generation, v1alpha1.ConditionReady, metav1.ConditionTrue, "Idle", "scenario is idle")
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("run finished", "runID", run.ID, "result", result)
		r.Recorder.Eventf(fs, corev1.EventTypeNormal, "RunFinished", "run %s finished: %s", run.ID, result)
	} else if fs.Status.Phase != v1alpha1.PhaseIdle {
		if err := r.patchStatus(ctx, fs, func(s *v1alpha1.FailureScenarioStatus) {
			s.Phase = v1alpha1.PhaseIdle
			meta.RemoveStatusCondition(&s.Conditions, v1alpha1.ConditionRevertFailed)
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileDelete drains active actions before releasing the finalizer. With
// the force-release annotation, remaining tokens are dumped into the
// leaked-state ConfigMap instead.
func (r *FailureScenarioReconciler) reconcileDelete(ctx context.Context, fs *v1alpha1.FailureScenario) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(fs, FinalizerName) {
		return ctrl.Result{}, nil
	}

	if fs.Annotations[ForceReleaseAnnotation] == "true" && len(fs.Status.ActiveActions) > 0 {
		log.Info("force-release requested: leaking revert tokens to configmap",
			"configmap", LeakedStateConfigMap, "actions", len(fs.Status.ActiveActions))
		if err := r.leakState(ctx, fs); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(fs, FinalizerName)
		return ctrl.Result{}, r.Update(ctx, fs)
	}

	return r.reconcileRevert(ctx, fs, fs.Status.CurrentRun, true)
}

// leakState appends the scenario's remaining revert tokens to the
// rca-lab-leaked-state ConfigMap.
func (r *FailureScenarioReconciler) leakState(ctx context.Context, fs *v1alpha1.FailureScenario) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm := &corev1.ConfigMap{}
		key := types.NamespacedName{Namespace: r.Namespace, Name: LeakedStateConfigMap}
		err := r.Get(ctx, key, cm)
		create := apierrors.IsNotFound(err)
		if err != nil && !create {
			return err
		}
		cm.Name = key.Name
		cm.Namespace = key.Namespace
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		ts := time.Now().UTC().Format("20060102-150405")
		for _, aa := range fs.Status.ActiveActions {
			cm.Data[fmt.Sprintf("%s.%s.%s", fs.Name, aa.Name, ts)] = string(aa.Revert.Raw)
		}
		if create {
			return r.Create(ctx, cm)
		}
		return r.Update(ctx, cm)
	})
}

func (r *FailureScenarioReconciler) runContext(fs *v1alpha1.FailureScenario, run *v1alpha1.CurrentRun) actions.RunContext {
	rc := actions.RunContext{
		ScenarioName:  fs.Name,
		ScenarioUID:   fs.UID,
		Namespace:     fs.Namespace,
		OperatorImage: r.OperatorImage,
	}
	if run != nil {
		rc.RunID = run.ID
		if run.ExpiresAt != nil {
			rc.RunDuration = run.ExpiresAt.Sub(run.StartedAt.Time)
		}
	}
	return rc
}

// patchStatus applies a status mutation with retry-on-conflict and refreshes
// fs with the persisted object. Returning nil guarantees the mutation is
// durable in the API server (the plan->persist->mutate invariant relies on
// this).
func (r *FailureScenarioReconciler) patchStatus(ctx context.Context, fs *v1alpha1.FailureScenario, mutate func(*v1alpha1.FailureScenarioStatus)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.FailureScenario{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(fs), latest); err != nil {
			return err
		}
		mutate(&latest.Status)
		latest.Status.ObservedGeneration = latest.Generation
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}
		*fs = *latest
		return nil
	})
}

// SetupWithManager wires the controller into the manager.
func (r *FailureScenarioReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.FailureScenario{}).
		Owns(&batchv1.Job{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Named("failurescenario").
		Complete(r)
}
