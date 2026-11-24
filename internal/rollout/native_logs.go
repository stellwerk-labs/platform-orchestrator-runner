package rollout

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"filippo.io/age"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
)

func (e *Engine) logAuthority(ctx context.Context, tx *sql.Tx, command Command, p Progress, phase, subject string) (nativelogs.Authority, error) {
	var commandDigest string
	if err := tx.QueryRowContext(ctx, `SELECT digest FROM rollout_commands WHERE id=$1`, command.ID).Scan(&commandDigest); err != nil {
		return nativelogs.Authority{}, err
	}
	return commandLogAuthority(command, commandDigest, phase, subject, p.Revision+1), nil
}

func commandLogAuthority(command Command, commandDigest, phase, subject string, revision int64) nativelogs.Authority {
	a := nativelogs.Authority{OrganizationID: command.OrgID, EnvironmentID: command.EnvironmentID, DeploymentID: command.ID, CommandDigest: commandDigest, Target: *command.NativeTarget, Phase: phase, ExecutionID: command.ID, CaptureRevision: revision, Subject: subject}
	if command.SourcePlan != nil {
		a.SourcePlanID, a.SourceSubject = command.SourcePlan.SourcePlanID, command.SourcePlan.Subject
		if phase == "plan" {
			a.Inherited, a.ExecutionID = true, a.SourcePlanID
		}
	}
	return a
}

// An invalid export recipient is finite export unavailability. Site encryption
// is still required, and failures of it or the journal remain storage errors.
func (e *Engine) prepareNativeLogs(authority nativelogs.Authority, recipient string, logs []byte, tooLarge bool) ([]byte, nativelogs.Descriptor, []byte, error) {
	descriptor := nativelogs.Descriptor{Version: 1, Authority: authority, Status: nativelogs.NotManaged}
	if tooLarge || len(logs) > nativelogs.RawLimit {
		logs, descriptor.Status = nil, nativelogs.TooLarge
	} else if recipient != "" {
		descriptor.Status = nativelogs.PendingTransfer
	}
	envelope := nativelogs.Envelope{Version: 1, Authority: authority, Status: descriptor.Status, Logs: logs}
	if descriptor.Status == nativelogs.PendingTransfer {
		var ciphertext []byte
		descriptor, ciphertext = e.prepareNativeLogExport(envelope, recipient)
		if ciphertext != nil {
			return ciphertext, descriptor, ciphertext, nil
		}
		// Export size/serialization failures retain the complete site-only bytes.
		envelope.Status = nativelogs.Unavailable
	}

	local, err := nativelogs.Encode(envelope, e.executor.Identity.Recipient())
	return local, descriptor, nil, err
}

func (e *Engine) prepareNativeLogExport(envelope nativelogs.Envelope, recipient string) (nativelogs.Descriptor, []byte) {
	unavailable := nativelogs.Descriptor{Version: 1, Authority: envelope.Authority, Status: nativelogs.Unavailable}
	managed, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return unavailable, nil
	}
	ciphertext, err := nativelogs.Encode(envelope, e.executor.Identity.Recipient(), managed)
	if err != nil {
		if errors.Is(err, nativelogs.ErrTooLarge) {
			unavailable.Status = nativelogs.TooLarge
		}
		return unavailable, nil
	}
	descriptor := unavailable
	descriptor.Status = nativelogs.PendingTransfer
	descriptor.CiphertextSHA256, descriptor.CiphertextBytes = nativelogs.Hash(ciphertext), len(ciphertext)
	descriptor.ObjectKey = envelope.Authority.ObjectKey(descriptor.CiphertextSHA256)
	attachment, err := json.Marshal(LogAttachment{Descriptor: descriptor, Ciphertext: ciphertext})
	if err != nil {
		return unavailable, nil
	}
	if len(attachment) > nativelogs.DeliveryLimit-(4<<20) {
		unavailable.Status = nativelogs.TooLarge
		return unavailable, nil
	}
	if descriptor.Validate() != nil {
		return unavailable, nil
	}
	return descriptor, ciphertext
}

func (e *Engine) saveLogCapture(ctx context.Context, tx *sql.Tx, p *Progress, descriptor nativelogs.Descriptor, ciphertext []byte) error {
	if descriptor.Validate() != nil || descriptor.Authority.DeploymentID != p.ID || descriptor.Authority.EnvironmentID != p.EnvironmentID || descriptor.Authority.CaptureRevision != p.Revision+1 {
		return nativelogs.ErrArtifact
	}
	p.LogCapture = &descriptor
	if err := saveProgress(ctx, tx, p); err != nil {
		return err
	}
	var eventID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM rollout_events WHERE rollout=$1 AND revision=$2`, p.ID, p.Revision).Scan(&eventID); err != nil {
		return err
	}
	var attachment *LogAttachment
	if len(ciphertext) != 0 {
		attachment = &LogAttachment{Descriptor: descriptor, Ciphertext: ciphertext}
	}
	_, err := e.prepareDeliveryInTransaction(ctx, tx, eventID, attachment)
	return err
}

func (e *Engine) decryptLogs(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id, phase, subject string, content []byte) ([]byte, error) {
	command, err := e.loadCommand(ctx, query, id)
	if err != nil {
		return nil, err
	}
	if command.NativeTarget == nil {
		return e.decrypt(content)
	}
	if len(content) > nativelogs.CiphertextLimit {
		return nil, nativelogs.ErrArtifact
	}
	plain, err := nativelogs.Decrypt(content, e.executor.Identity, nativelogs.EnvelopeLimit)
	if err != nil {
		return nil, err
	}
	// Old Native commands have raw site-only log ciphertext. Its bytes remain
	// valid without rewriting retained commands or inventing an export recipient.
	if !bytes.HasPrefix(plain, []byte(`{"version":1,"authority":`)) {
		if command.Request.EncryptedLogsRecipient != "" || len(plain) > nativelogs.RawLimit {
			return nil, nativelogs.ErrArtifact
		}
		return plain, nil
	}
	var envelope nativelogs.Envelope
	if nativelogs.StrictJSON(plain, &envelope, nativelogs.EnvelopeLimit) != nil {
		return nil, nativelogs.ErrArtifact
	}
	var commandDigest string
	if err = query.QueryRowContext(ctx, `SELECT digest FROM rollout_commands WHERE id=$1`, id).Scan(&commandDigest); err != nil {
		return nil, err
	}
	if phase == "apply" {
		progress, progressErr := readProgress(ctx, query, id)
		if progressErr != nil {
			return nil, progressErr
		}
		subject = progress.Subject
	}
	authority := commandLogAuthority(command, commandDigest, phase, subject, envelope.Authority.CaptureRevision)
	// Decode compares every retained authority field and rejects unknown status,
	// duplicate/trailing data and oversize raw bytes without returning partial text.
	checked, err := nativelogs.DecodePlain(plain, authority)
	if err != nil {
		return nil, err
	}
	if checked.Status == nativelogs.TooLarge {
		return nil, refused("plan_logs_too_large")
	}
	return checked.Logs, nil
}
