// Package rolloutagent connects the opt-in local coordinator to the Runner Gateway.
package rolloutagent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/gatewayapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
)

const CommandType = "execute-rollout-v1"
const EventType = "rollout-progress"

// Run acknowledges handoff after durable import. Execution does not depend on this loop.
func Run(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, org, site string) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		flushEvents(ctx, engine, client, strings.HasPrefix(site, hmessaging.NativeRunnerTokenPrefix))
	}()
	defer func() { <-done }()
	for ctx.Err() == nil {
		if !engine.Accepting() {
			pause(ctx)
			continue
		}
		delivery, err := client.NextCommand(ctx)
		if err != nil {
			if !errors.Is(err, gatewayapi.ErrNoCommand) {
				reportFailure("fetch_command", err)
				pause(ctx)
			}
			continue
		}
		operation, err := handleDelivery(ctx, engine, client, org, site, delivery)
		if err != nil {
			reportFailure(operation, err)
		}
	}
}

func flushEvents(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, native bool) {
	done := make(chan struct{})
	go func() { defer close(done); flushLogEvents(ctx, engine, client) }()
	defer func() { <-done }()
	sweep := controlSweep{}
	cursor := eventCursor{}
	for ctx.Err() == nil {
		if native {
			sweep.flush(ctx, engine, client)
		}
		pageCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		flushProgressPage(pageCtx, engine, client, &cursor)
		cancel()
		pause(ctx)
	}
}

// Each finite pass captures the journal's high-water ID. New arrivals cannot
// postpone failed-row retries forever, and pages still advance past failures.
type eventCursor struct{ after, through int64 }

func (c *eventCursor) page(ctx context.Context, engine *rollout.Engine) ([]rollout.Event, error) {
	if c.after >= c.through {
		c.after, c.through = 0, 0
	}
	if c.through == 0 {
		bound, err := engine.PendingEventHighWater(ctx)
		if err != nil {
			return nil, err
		}
		c.through = bound
	}
	events, err := engine.Events(ctx, c.after)
	if err != nil {
		return nil, err
	}
	end := 0
	for end < len(events) && events[end].ID <= c.through {
		end++
	}
	events = events[:end]
	if len(events) == 0 {
		c.after, c.through = 0, 0
	}
	return events, nil
}

// The cursor advances even when publication fails. Empty pages restart a pass,
// so a retained failed row cannot hide later progress or starve its own retry.
func flushProgressPage(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, cursor *eventCursor) {
	events, err := cursor.page(ctx, engine)
	if err != nil {
		reportFailure("list_events", err)
		return
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return
		}
		cursor.after = event.ID
		attempt, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		delivery, prepareErr := engine.PrepareDelivery(attempt, event.ID)
		if prepareErr == nil {
			prepareErr = publishProgress(attempt, engine, client, delivery)
		}
		if prepareErr == nil && delivery.Log == nil {
			prepareErr = engine.AckEvent(attempt, event.ID)
		}
		cancel()
		if prepareErr != nil {
			reportFailure("publish_event", prepareErr)
		}
	}
}

// One artifact attempt per iteration bounds work without a second durable queue.
// The existing journal remains custody; ordinary/control publication never waits
// on the independently cancellable full-artifact HTTP budget.
func flushLogEvents(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client) {
	cursor := eventCursor{}
	for ctx.Err() == nil {
		flushLogPage(ctx, engine, client, &cursor)
		pause(ctx)
	}
}

func flushLogPage(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, cursor *eventCursor) {
	events, err := cursor.page(ctx, engine)
	if err != nil {
		reportFailure("list_log_events", err)
		return
	}
	for _, event := range events {
		if ctx.Err() != nil {
			return
		}
		cursor.after = event.ID
		if event.Progress.LogCapture == nil || event.Progress.LogCapture.Status != nativelogs.PendingTransfer {
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, 4*time.Minute)
		operation, publishErr := publishEvent(attempt, engine, client, event)
		cancel()
		if publishErr != nil {
			reportFailure(operation, publishErr)
		}
		return
	}
}

func publishProgress(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, delivery rollout.EventDelivery) error {
	if !engine.Ready() {
		return errors.New("local_runtime_unavailable")
	}
	if err := client.PublishEventBytes(ctx, delivery.Progress); err != nil {
		return err
	}
	if len(delivery.Result) > 0 {
		return client.PublishEventBytes(ctx, delivery.Result)
	}
	return nil
}

func publishEvent(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, event rollout.Event) (string, error) {
	delivery, err := engine.PrepareDelivery(ctx, event.ID)
	if err != nil {
		return "prepare_event", err
	}
	if err = publishProgress(ctx, engine, client, delivery); err != nil {
		return "publish_progress", err
	}
	if delivery.Log != nil {
		if err = client.PutNativeLog(ctx, delivery.Log.Descriptor, delivery.Log.Ciphertext); err != nil {
			return "upload_log", err
		}
		ready, readyErr := logReadyBytes(delivery)
		if readyErr != nil {
			return "prepare_log_ready", readyErr
		}
		if err = client.PublishEventBytes(ctx, ready); err != nil {
			return "publish_log_ready", err
		}
	}
	return "ack_event", engine.AckEvent(ctx, event.ID)
}

func logReadyBytes(delivery rollout.EventDelivery) ([]byte, error) {
	if delivery.Log == nil || delivery.Log.Descriptor.VerifyCiphertext(delivery.Log.Ciphertext) != nil {
		return nil, nativelogs.ErrArtifact
	}
	var progress hmessaging.EventEnvelope
	if json.Unmarshal(delivery.Progress, &progress) != nil {
		return nil, nativelogs.ErrArtifact
	}
	payload, err := json.Marshal(delivery.Log.Descriptor)
	if err != nil {
		return nil, err
	}
	ready := hmessaging.EventEnvelope{ProtocolVersion: progress.ProtocolVersion, EventID: delivery.Log.Descriptor.EventID(),
		OrganizationID: progress.OrganizationID, RunnerID: progress.RunnerID, DeploymentID: progress.DeploymentID,
		Type: nativelogs.ReadyEventType, CreatedAt: progress.CreatedAt, Payload: payload}
	data, err := json.Marshal(ready)
	if err != nil {
		return nil, err
	}
	if _, _, err = nativelogs.ParseReady(data); err != nil {
		return nil, err
	}
	return data, nil
}
func handleDelivery(ctx context.Context, engine *rollout.Engine, client *gatewayapi.Client, org, site string, delivery gatewayapi.CommandResponse) (string, error) {
	var envelope hmessaging.CommandEnvelope
	_ = json.Unmarshal(delivery.Command, &envelope)
	if envelope.Type == ControlCommandType {
		return handleControlDelivery(ctx, engine, client, org, site, delivery)
	}
	command, signed, err := decodeDelivery(delivery, org, site)
	if err == nil {
		_, err = engine.Submit(ctx, signed)
	}
	if err == nil {
		return "ack_command", client.Acknowledge(ctx, command.CommandID, delivery.Receipt)
	}
	var refusal *rollout.Error
	if errors.As(err, &refusal) {
		return "reject_command", client.Reject(ctx, command.CommandID, delivery.Receipt, refusal.Code)
	}
	return "retry_command", client.Retry(ctx, command.CommandID, delivery.Receipt, time.Second)
}

func reportFailure(operation string, err error) {
	class, status := "local_or_transport", 0
	var httpError *gatewayapi.HTTPStatusError
	switch {
	case errors.As(err, &httpError):
		class, status = "http", httpError.StatusCode
	case errors.Is(err, context.Canceled):
		class = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		class = "timeout"
	}
	slog.Warn("Runner delivery outcome is unconfirmed", "operation", operation, "class", class, "status", status, "code", "delivery_unconfirmed")
}
func decodeDelivery(delivery gatewayapi.CommandResponse, org, site string) (hmessaging.CommandEnvelope, rollout.SignedCommand, error) {
	var command hmessaging.CommandEnvelope
	var signed rollout.SignedCommand
	if json.Unmarshal(delivery.Command, &command) != nil || command.ProtocolVersion != hmessaging.ProtocolVersionV1 || command.OrganizationID != org || command.RunnerID != site || command.Type != CommandType {
		return command, signed, &rollout.Error{Code: "command_scope_refused"}
	}
	if json.Unmarshal(command.Payload, &signed) != nil {
		return command, signed, &rollout.Error{Code: "invalid_command"}
	}
	var scope struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(signed.Payload, &scope) != nil || scope.ID == "" || scope.ID != command.DeploymentID {
		return command, signed, &rollout.Error{Code: "command_scope_refused"}
	}
	return command, signed, nil
}

func pause(ctx context.Context) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
