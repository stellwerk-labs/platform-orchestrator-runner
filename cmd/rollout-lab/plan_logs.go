package main

import (
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

func (l *lab) planLogs(w http.ResponseWriter, r *http.Request) { l.artifactLogs(w, r, "plan-logs") }
func (l *lab) executionLogs(w http.ResponseWriter, r *http.Request) {
	l.artifactLogs(w, r, "execution-logs")
}
func (l *lab) artifactLogs(w http.ResponseWriter, r *http.Request, kind string) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	subject := r.URL.Query().Get("subject")
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, l.cfg.OperatorURL+"/v1/rollouts/"+id.String()+"/"+kind+"?subject="+url.QueryEscape(subject), nil)
	if err != nil {
		reply(w, 503, map[string]string{"error": "site_logs_unavailable"})
		return
	}
	request.Header.Set("Authorization", "Bearer "+l.token)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		reply(w, 503, map[string]string{"error": "site_logs_unavailable"})
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		status := response.StatusCode
		if status != 404 && status != 409 && status != 413 {
			status = 503
		}
		reply(w, status, map[string]string{"error": "site_logs_unavailable"})
		return
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, rolloutexec.PlanLogLimit+1))
	if len(content) > rolloutexec.PlanLogLimit {
		reply(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "plan_logs_too_large"})
		return
	}
	if err != nil || response.Header.Get("X-Stellwerk-Plan-Subject") != subject {
		reply(w, 503, map[string]string{"error": "site_logs_unavailable"})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Stellwerk-Plan-Subject", subject)
	_, _ = w.Write(content)
}
