package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
)

// Import refusals have no execution history. Project the existing durable dead letter.
func (l *lab) projectRefusals(ctx context.Context, connection *nats.Conn) {
	commands, _ := hmessaging.RunnerCommandSubject(l.cfg.OrgID, l.cfg.RunnerID)
	subject, _ := hmessaging.DeadLetterSubject(commands)
	consumer, err := natstransport.NewBoundConsumer(connection, hmessaging.DeadLettersStreamName, subject, "dbos-lab-refusals")
	if err != nil {
		return
	}
	for ctx.Err() == nil {
		delivery, err := consumer.Fetch(ctx)
		if err != nil {
			continue
		}
		var command hmessaging.CommandEnvelope
		if json.Unmarshal(delivery.Message.Data, &command) != nil {
			_ = delivery.Term()
			continue
		}
		reason := delivery.Message.Header.Get("Po-Dead-Letter-Reason")
		if err = l.recordRefusal(command, reason); err != nil {
			_ = delivery.Nak(time.Second)
		} else {
			_ = delivery.Ack()
		}
	}
}
func (l *lab) recordRefusal(command hmessaging.CommandEnvelope, code string) error {
	if command.OrganizationID != l.cfg.OrgID || command.RunnerID != l.cfg.RunnerID {
		return nil
	}
	if _, err := uuid.Parse(command.DeploymentID); err != nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var original submission
	data, err := os.ReadFile(filepath.Join(l.cfg.Directory, "imports", command.DeploymentID+".json"))
	if err != nil {
		return nil
	}
	if json.Unmarshal(data, &original) != nil || !bytes.Equal(command.Payload, original.Envelope.Payload) {
		return nil
	}
	p, err := l.read(command.DeploymentID)
	if err != nil || p.Revision > 0 {
		return err
	}
	safe := []string{"signature_refused", "command_expired", "catalog_refused", "steps_refused", "capability_refused", "environment_busy", "request_scope_refused", "command_scope_refused", "command_content_conflict"}
	if !slices.Contains(safe, code) {
		code = "import_refused"
	}
	p.Phase, p.ErrorCode, p.Revision, p.LastReceivedAt = "refused", code, 1, time.Now().UTC()
	return atomicJSON(filepath.Join(l.cfg.Directory, "projections", p.ID+".json"), p)
}
