package rollout

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
)

// LogAttachment is one opaque capture snapshot, retained only in the encrypted
// local journal. Transport publishes its descriptor, never these bytes, to NATS.
type LogAttachment struct {
	Descriptor nativelogs.Descriptor `json:"descriptor"`
	Ciphertext []byte                `json:"ciphertext"`
}

// EventDelivery freezes exact envelopes before publishing. An absent result is
// intentional and never filled in on replay.
type EventDelivery struct {
	Log      *LogAttachment  `json:"log,omitempty"`
	Version  int             `json:"version"`
	EventID  int64           `json:"event_id"`
	Progress json.RawMessage `json:"progress"`
	Result   json.RawMessage `json:"result,omitempty"`
}

func (e *Engine) PrepareDelivery(ctx context.Context, id int64) (EventDelivery, error) {
	tx, err := e.beginEvent(ctx, id)
	if err != nil {
		return EventDelivery{}, err
	}
	defer func() { _ = tx.Rollback() }()
	delivery, err := e.prepareDeliveryInTransaction(ctx, tx, id, nil)
	if err != nil {
		return EventDelivery{}, err
	}
	return delivery, tx.Commit()
}

func (e *Engine) prepareDeliveryInTransaction(ctx context.Context, tx *sql.Tx, id int64, attachment *LogAttachment) (EventDelivery, error) {
	var content []byte
	err := tx.QueryRowContext(ctx, `SELECT content FROM rollout_deliveries WHERE event_id=$1`, id).Scan(&content)
	if err == nil {
		return e.readDelivery(id, content)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EventDelivery{}, err
	}
	delivery, err := e.freezeDelivery(ctx, tx, id)
	if err != nil {
		return EventDelivery{}, err
	}
	// The ordinary envelope retains its 4 MiB budget. Only the additional local
	// attachment gets the separately bounded journal allowance.
	base, err := json.Marshal(delivery)
	if err != nil || len(base) > 4<<20 {
		return EventDelivery{}, refused("delivery_too_large")
	}
	delivery.Log = attachment
	if attachment != nil && attachment.Descriptor.VerifyCiphertext(attachment.Ciphertext) != nil {
		return EventDelivery{}, refused("delivery_scope_refused")
	}
	plain, err := json.Marshal(delivery)
	limit := 4 << 20
	if attachment != nil {
		limit = nativelogs.DeliveryLimit
	}
	if err != nil || len(plain) > limit {
		return EventDelivery{}, refused("delivery_too_large")
	}
	content, err = e.encrypt(plain)
	if err != nil {
		return EventDelivery{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rollout_deliveries(event_id,content,result_owner)
 SELECT id,$1,CASE WHEN $2 THEN rollout ELSE NULL END FROM rollout_events WHERE id=$3`, content, len(delivery.Result) > 0, id); err != nil {
		return EventDelivery{}, err
	}
	return delivery, nil
}

func (e *Engine) freezeDelivery(ctx context.Context, tx *sql.Tx, id int64) (EventDelivery, error) {
	var rolloutID string
	var revision int64
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT rollout,revision,payload FROM rollout_events WHERE id=$1`, id).Scan(&rolloutID, &revision, &payload); err != nil {
		return EventDelivery{}, err
	}
	command, result, ready, err := e.completionForRevision(ctx, tx, rolloutID, revision)
	if err != nil {
		return EventDelivery{}, err
	}
	var progress Progress
	if json.Unmarshal(payload, &progress) != nil || progress.ID != rolloutID || progress.Revision != revision || progress.EnvironmentID != command.EnvironmentID {
		return EventDelivery{}, refused("delivery_scope_refused")
	}
	body, err := json.Marshal(struct {
		ID       int64           `json:"id"`
		Progress json.RawMessage `json:"progress"`
	}{id, payload})
	if err != nil {
		return EventDelivery{}, err
	}
	now := time.Now().UTC()
	delivery := EventDelivery{Version: 1, EventID: id}
	delivery.Progress, err = e.eventEnvelope(rolloutID, rolloutID+":"+strconv.FormatInt(revision, 10), "rollout-progress", now, body)
	if err == nil && ready {
		delivery.Result, err = e.resultEnvelope(ctx, tx, rolloutID, now, result)
	}
	return delivery, err
}

func (e *Engine) resultEnvelope(ctx context.Context, tx *sql.Tx, id string, created time.Time, payload []byte) ([]byte, error) {
	var owned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rollout_deliveries WHERE result_owner=$1)`, id).Scan(&owned); err != nil {
		return nil, err
	}
	if owned {
		return nil, nil
	}
	return e.eventEnvelope(id, id+":deployment-result", "deployment-result", created, payload)
}

func (e *Engine) eventEnvelope(id, eventID, kind string, created time.Time, payload []byte) ([]byte, error) {
	transportID := e.config.RunnerID
	if e.config.RunnerUUID != "" {
		var err error
		transportID, err = hmessaging.NativeRunnerToken(e.config.RunnerUUID)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(hmessaging.EventEnvelope{ProtocolVersion: hmessaging.ProtocolVersionV1, EventID: eventID,
		OrganizationID: e.config.OrgID, RunnerID: transportID, DeploymentID: id, Type: kind, CreatedAt: created, Payload: payload})
}

func (e *Engine) readDelivery(id int64, content []byte) (EventDelivery, error) {
	if len(content) > nativelogs.DeliveryLimit+(64<<10) {
		return EventDelivery{}, refused("delivery_unavailable")
	}
	plain, err := nativelogs.Decrypt(content, e.executor.Identity, nativelogs.DeliveryLimit)
	if err != nil {
		return EventDelivery{}, refused("delivery_unavailable")
	}
	var delivery EventDelivery
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&delivery) != nil || decoder.Decode(new(any)) != io.EOF || delivery.Version != 1 || delivery.EventID != id || !json.Valid(delivery.Progress) || len(delivery.Result) > 0 && !json.Valid(delivery.Result) {
		return EventDelivery{}, refused("delivery_unavailable")
	}
	if delivery.Log == nil {
		if len(plain) > 4<<20 {
			return EventDelivery{}, refused("delivery_unavailable")
		}
		return delivery, nil
	}
	// Only log authority is duplicate-field strict. Private result metadata keeps
	// its existing case-sensitive customer map keys and legacy decoder behavior.
	var raw struct {
		Log json.RawMessage `json:"log"`
	}
	if json.Unmarshal(plain, &raw) != nil || nativelogs.StrictJSON(raw.Log, delivery.Log, nativelogs.DeliveryLimit) != nil {
		return EventDelivery{}, refused("delivery_unavailable")
	}
	baseDelivery := delivery
	baseDelivery.Log = nil
	base, marshalErr := json.Marshal(baseDelivery)
	if marshalErr != nil || len(base) > 4<<20 {
		return EventDelivery{}, refused("delivery_unavailable")
	}
	if delivery.Log.Descriptor.VerifyCiphertext(delivery.Log.Ciphertext) != nil || len(delivery.Progress) > 4<<20 || len(delivery.Result) > 4<<20 {
		return EventDelivery{}, refused("delivery_unavailable")
	}
	var envelope hmessaging.EventEnvelope
	var event Event
	if json.Unmarshal(delivery.Progress, &envelope) != nil || json.Unmarshal(envelope.Payload, &event) != nil || event.ID != id || event.Progress.LogCapture == nil || *event.Progress.LogCapture != delivery.Log.Descriptor {
		return EventDelivery{}, refused("delivery_unavailable")
	}
	return delivery, nil
}
