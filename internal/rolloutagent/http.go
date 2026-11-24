package rolloutagent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
)

// Handler is a private site-local operator endpoint, not central IAM authorization.
func Handler(engine *rollout.Engine, token string, catalog any) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/worker/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, engine.WorkerStatus()) })
	mux.HandleFunc("POST /v1/worker/drain", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, engine.Drain()) })
	mux.HandleFunc("GET /v1/worker/ready", func(w http.ResponseWriter, r *http.Request) {
		state := engine.WorkerStatus()
		status := http.StatusOK
		if !state.Accepting {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, state)
	})
	mux.HandleFunc("GET /v1/rollouts/{id}/plan-logs", planLogsHandler(engine))
	mux.HandleFunc("GET /v1/rollouts/{id}/execution-logs", executionLogsHandler(engine))
	mux.HandleFunc("GET /v1/catalog", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, catalog) })
	mux.HandleFunc("GET /v1/rollouts", func(w http.ResponseWriter, r *http.Request) { p, err := engine.List(r.Context()); respond(w, p, err) })
	mux.HandleFunc("GET /v1/rollouts/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, err := engine.Get(r.Context(), r.PathValue("id"))
		respond(w, p, err)
	})
	mux.HandleFunc("POST /v1/rollouts/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Subject string `json:"subject"`
			StepID  string `json:"step_id"`
			Actor   string `json:"actor"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || body.Actor == "" {
			writeJSON(w, 400, map[string]string{"error": "invalid_operator_request"})
			return
		}
		var p rollout.Progress
		var err error
		switch r.PathValue("action") {
		case "approve":
			p, err = engine.Decide(r.Context(), r.PathValue("id"), body.Subject, "approve", body.Actor)
		case "cancel":
			p, err = engine.Cancel(r.Context(), r.PathValue("id"), body.Actor)
		case "repair":
			p, err = engine.Repair(r.Context(), r.PathValue("id"), body.StepID, body.Actor)
		default:
			writeJSON(w, 404, map[string]string{"error": "unknown_action"})
			return
		}
		respond(w, p, err)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "site_authorization_required"})
			return
		}
		if !engine.Ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "local_runtime_unavailable"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func respond(w http.ResponseWriter, value any, err error) {
	if err == nil {
		writeJSON(w, 200, value)
		return
	}
	var refusal *rollout.Error
	if errors.As(err, &refusal) {
		writeJSON(w, 409, map[string]string{"error": refusal.Code})
		return
	}
	writeJSON(w, 503, map[string]string{"error": "local_runtime_unavailable"})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func planLogsHandler(engine *rollout.Engine) http.HandlerFunc {
	return artifactLogsHandler(engine.GetPlanLogs)
}
func executionLogsHandler(engine *rollout.Engine) http.HandlerFunc {
	return artifactLogsHandler(engine.GetExecutionLogs)
}
func artifactLogsHandler(load func(context.Context, string, string) (rollout.PlanLogs, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logs, err := load(r.Context(), r.PathValue("id"), r.URL.Query().Get("subject"))
		if err != nil {
			status := http.StatusServiceUnavailable
			var refusal *rollout.Error
			if errors.As(err, &refusal) {
				switch refusal.Code {
				case "plan_logs_unavailable":
					status = http.StatusNotFound
				case "plan_logs_subject_changed":
					status = http.StatusConflict
				case "plan_logs_too_large":
					status = http.StatusRequestEntityTooLarge
				}
			}
			writeJSON(w, status, map[string]string{"error": "plan_logs_unavailable"})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Stellwerk-Plan-Subject", logs.Subject)
		_, _ = w.Write(logs.Content)
	}
}
