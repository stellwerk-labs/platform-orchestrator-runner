package rollout

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var errStorageUnavailable = errors.New("storage_unavailable")
var errGenerationSuperseded = errors.New("worker_generation_superseded")

// Fatal reports a worker failure, never a deployment business refusal. The host
// must call Close before cancelling a DBOS parent or waiting for worker loops.
func (e *Engine) Fatal() <-chan error { return e.failure }

func (e *Engine) Ready() bool { return e.running.Load() && !e.stopped.Load() && !e.closed.Load() }

func (e *Engine) stopWorker(reason error) {
	if e.config.Postgres != nil && e.running.Load() && e.stopped.CompareAndSwap(false, true) {
		e.failure <- reason
	}
}

// No query arguments, private errors or SQL enter logs or durable checkpoints.
type committingQueryKey struct{}
type callerQueryContextKey struct{}
type originalCallerContextKey struct{}

func (e *Engine) transactionClient(ctx context.Context) (dbos.Context, context.CancelFunc) {
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	bounded = context.WithValue(bounded, originalCallerContextKey{}, ctx)
	// DBOS deliberately strips cancellation for its checkpoint operations.
	// Preserve only this caller-owned SQL operation's context as a public value.
	return dbos.WithValue(e.durable, callerQueryContextKey{}, bounded), cancel
}

func (e *Engine) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if caller, ok := ctx.Value(callerQueryContextKey{}).(context.Context); ok {
		ctx = caller
	}
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		return context.WithValue(ctx, committingQueryKey{}, true)
	}
	return ctx
}

func (e *Engine) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if e.ignoreCancellation(ctx) {
		return
	}
	if data.CommandTag.String() == "ROLLBACK" && ctx.Value(committingQueryKey{}) == true {
		e.stopWorker(errStorageUnavailable)
		return
	}
	if data.Err == nil {
		return
	}
	var pgError *pgconn.PgError
	if errors.As(data.Err, &pgError) && pgError.Code == "23505" && (pgError.ConstraintName == "rollout_environments_pkey" || pgError.ConstraintName == "rollout_environments_rollout_key") {
		return
	}
	e.stopWorker(errStorageUnavailable)
}

func (e *Engine) interrupted() error {
	if !e.stopped.Load() {
		return nil
	}
	// The root DBOS Shutdown cause preserves PENDING. A step-local timeout or
	// parent cancellation would durably poison recovery instead.
	<-e.durable.Done()
	return e.durable.Err()
}

func (e *Engine) ignoreCancellation(ctx context.Context) bool {
	if original, ok := ctx.Value(originalCallerContextKey{}).(context.Context); ok {
		ctx = original
	}
	return ctx.Err() != nil && (e.closed.Load() || ctx.Value(workflowOwnerKey{}) == nil)
}

func (e *Engine) storageFailed(ctx context.Context) {
	if !e.ignoreCancellation(ctx) {
		e.stopWorker(errStorageUnavailable)
	}
}
