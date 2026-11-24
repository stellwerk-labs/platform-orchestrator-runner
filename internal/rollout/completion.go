package rollout

import (
	"context"
	"database/sql"
	"encoding/json"

	api "github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
)

func (e *Engine) completionForRevision(ctx context.Context, tx *sql.Tx, id string, revision int64) (Command, []byte, bool, error) {
	command, err := e.commandInTransaction(ctx, tx, id)
	if err != nil {
		return Command{}, nil, false, err
	}
	p, err := readProgress(ctx, tx, id)
	if err != nil {
		return Command{}, nil, false, err
	}
	if p.ID != command.ID || p.EnvironmentID != command.EnvironmentID {
		return Command{}, nil, false, refused("completion_scope_refused")
	}
	if command.Version != e.pipelineVersion() || p.Revision != revision || !terminal(p.Phase) || p.Phase == "outcome_unknown" {
		return command, nil, false, nil
	}
	body, err := e.completionBody(ctx, tx, command, p)
	return command, body, err == nil, err
}

func (e *Engine) completionBody(ctx context.Context, tx *sql.Tx, command Command, p Progress) ([]byte, error) {
	if p.Phase == "completed" || p.InfrastructureApplied {
		var body []byte
		if err := tx.QueryRowContext(ctx, `SELECT body FROM rollout_pipeline_results WHERE rollout=$1`, p.ID).Scan(&body); err != nil {
			return nil, err
		}
		return e.decrypt(body)
	}
	receipt, err := e.noEffectReceipt(ctx, tx, command)
	if err != nil {
		return nil, err
	}
	message := p.ErrorCode
	if message == "" {
		message = p.Phase
	}
	return json.Marshal(api.DeploymentResultsUpdateBody{Status: api.Failure, Error: &api.Error{Error: "PIPELINE", Message: message}, NoEffectReceipt: receipt})
}
