// Package api serves a small REST API and an embedded web UI for driving
// FailureScenario resources. The Server is a controller-runtime
// manager.Runnable: it runs an http.Server on the configured address using the
// manager's cached client for reads and writes, and shuts down cleanly when the
// manager's context is cancelled.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/coroot/rca-lab/operator/api/v1alpha1"
)

// scenarioNamespace is the single namespace all scenarios live in.
const scenarioNamespace = "default"

// defaultRunDuration is used when POST /run omits durationSeconds.
const defaultRunDuration = 900 * time.Second

//go:embed web/index.html
var webFS embed.FS

// Server is a manager.Runnable that serves the REST API and web UI.
type Server struct {
	client client.Client
	addr   string
}

// NewServer builds a Server backed by the given client, listening on addr
// (e.g. ":8080").
func NewServer(c client.Client, addr string) *Server {
	return &Server{client: c, addr: addr}
}

// Start runs the HTTP server until ctx is cancelled, then shuts it down
// gracefully. It satisfies controller-runtime's manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("api")

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("starting API/UI server", "addr", s.addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		log.Info("shutting down API/UI server")
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// NeedLeaderElection reports that the server must run on every replica, not
// only the elected leader (single replica anyway; the UI must always serve).
func (s *Server) NeedLeaderElection() bool { return false }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/scenarios", s.handleList)
	mux.HandleFunc("GET /api/scenarios/{name}", s.handleGet)
	mux.HandleFunc("POST /api/scenarios/{name}/enable", s.handleEnable)
	mux.HandleFunc("POST /api/scenarios/{name}/disable", s.handleDisable)
	mux.HandleFunc("POST /api/scenarios/{name}/run", s.handleRun)
	mux.HandleFunc("POST /api/reset", s.handleReset)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Everything else serves the embedded UI (index.html at "/").
	content, err := fs.Sub(webFS, "web")
	if err != nil {
		// Only fails if the embed path is wrong, which is a build-time bug.
		panic(err)
	}
	fileServer := http.FileServer(http.FS(content))
	mux.Handle("GET /", http.StripPrefix("/", fileServer))

	return mux
}

// ---- REST handlers ----

// scenarioSummary is the list-view projection of a FailureScenario.
type scenarioSummary struct {
	Name               string               `json:"name"`
	DisplayName        string               `json:"displayName"`
	Description        string               `json:"description"`
	Category           string               `json:"category"`
	Severity           string               `json:"severity"`
	ExpectedSymptoms   []string             `json:"expectedSymptoms"`
	Enabled            bool                 `json:"enabled"`
	Phase              string               `json:"phase"`
	CurrentRun         *v1alpha1.CurrentRun `json:"currentRun"`
	LastCompletedRunID string               `json:"lastCompletedRunID"`
}

// scenarioDetail is the full detail view.
type scenarioDetail struct {
	scenarioSummary
	Duration      string                     `json:"duration,omitempty"`
	Actions       []v1alpha1.Action          `json:"actions"`
	ActiveActions []v1alpha1.ActiveAction    `json:"activeActions"`
	History       []v1alpha1.RunHistoryEntry `json:"history"`
	Conditions    []metav1.Condition         `json:"conditions"`
}

func summarize(fs *v1alpha1.FailureScenario) scenarioSummary {
	symptoms := fs.Spec.ExpectedSymptoms
	if symptoms == nil {
		symptoms = []string{}
	}
	phase := fs.Status.Phase
	if phase == "" {
		phase = v1alpha1.PhaseIdle
	}
	return scenarioSummary{
		Name:               fs.Name,
		DisplayName:        fs.Spec.DisplayName,
		Description:        fs.Spec.Description,
		Category:           fs.Spec.Category,
		Severity:           fs.Spec.Severity,
		ExpectedSymptoms:   symptoms,
		Enabled:            fs.Spec.Enabled,
		Phase:              phase,
		CurrentRun:         fs.Status.CurrentRun,
		LastCompletedRunID: fs.Status.LastCompletedRunID,
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	list := &v1alpha1.FailureScenarioList{}
	if err := s.client.List(r.Context(), list, client.InNamespace(scenarioNamespace)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]scenarioSummary, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, summarize(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	fs, err := s.get(r.Context(), r.PathValue("name"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	detail := scenarioDetail{
		scenarioSummary: summarize(fs),
		Actions:         fs.Spec.Actions,
		ActiveActions:   fs.Status.ActiveActions,
		History:         fs.Status.History,
		Conditions:      fs.Status.Conditions,
	}
	if fs.Spec.Duration != nil {
		detail.Duration = fs.Spec.Duration.Duration.String()
	}
	if detail.Actions == nil {
		detail.Actions = []v1alpha1.Action{}
	}
	if detail.ActiveActions == nil {
		detail.ActiveActions = []v1alpha1.ActiveAction{}
	}
	if detail.History == nil {
		detail.History = []v1alpha1.RunHistoryEntry{}
	}
	if detail.Conditions == nil {
		detail.Conditions = []metav1.Condition{}
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleEnable(w http.ResponseWriter, r *http.Request) {
	if err := s.mutate(r.Context(), r.PathValue("name"), func(fs *v1alpha1.FailureScenario) {
		fs.Spec.Enabled = true
	}); err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
}

func (s *Server) handleDisable(w http.ResponseWriter, r *http.Request) {
	if err := s.mutate(r.Context(), r.PathValue("name"), func(fs *v1alpha1.FailureScenario) {
		fs.Spec.Enabled = false
		fs.Spec.Trigger = nil
	}); err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

type runRequest struct {
	DurationSeconds int `json:"durationSeconds"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	duration := defaultRunDuration
	if r.ContentLength != 0 {
		var req runRequest
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.DurationSeconds < 0 {
			writeError(w, http.StatusBadRequest, "durationSeconds must be >= 0")
			return
		}
		if req.DurationSeconds > 0 {
			duration = time.Duration(req.DurationSeconds) * time.Second
		}
	}

	runID := uuid.NewString()
	if err := s.mutate(r.Context(), r.PathValue("name"), func(fs *v1alpha1.FailureScenario) {
		fs.Spec.Trigger = &v1alpha1.TriggerSpec{
			RunID:    runID,
			Duration: &metav1.Duration{Duration: duration},
		}
	}); err != nil {
		writeClientError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running", "runID": runID})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	list := &v1alpha1.FailureScenarioList{}
	if err := s.client.List(r.Context(), list, client.InNamespace(scenarioNamespace)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range list.Items {
		name := list.Items[i].Name
		if err := s.mutate(r.Context(), name, func(fs *v1alpha1.FailureScenario) {
			fs.Spec.Enabled = false
			fs.Spec.Trigger = nil
		}); err != nil {
			writeClientError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "reset", "count": len(list.Items)})
}

// ---- client helpers ----

func (s *Server) get(ctx context.Context, name string) (*v1alpha1.FailureScenario, error) {
	fs := &v1alpha1.FailureScenario{}
	key := types.NamespacedName{Namespace: scenarioNamespace, Name: name}
	if err := s.client.Get(ctx, key, fs); err != nil {
		return nil, err
	}
	return fs, nil
}

// mutate applies a spec mutation with retry-on-conflict. It always re-reads the
// latest object before applying the change.
func (s *Server) mutate(ctx context.Context, name string, apply func(*v1alpha1.FailureScenario)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fs := &v1alpha1.FailureScenario{}
		key := types.NamespacedName{Namespace: scenarioNamespace, Name: name}
		if err := s.client.Get(ctx, key, fs); err != nil {
			return err
		}
		apply(fs)
		return s.client.Update(ctx, fs)
	})
}

// ---- response helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeClientError maps a client error to an HTTP status (404 for NotFound,
// 500 otherwise).
func writeClientError(w http.ResponseWriter, err error) {
	if apierrors.IsNotFound(err) {
		writeError(w, http.StatusNotFound, "scenario not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
