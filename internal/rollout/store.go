package rollout

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"filippo.io/age"
)

const schema = `
CREATE TABLE IF NOT EXISTS rollout_site (
 id INTEGER PRIMARY KEY CHECK(id=1), org TEXT NOT NULL, runner TEXT NOT NULL, public_key TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_commands (
 id TEXT PRIMARY KEY, digest TEXT NOT NULL, payload BLOB NOT NULL, progress TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_environments (
 environment TEXT PRIMARY KEY, rollout TEXT NOT NULL UNIQUE REFERENCES rollout_commands(id)
);
CREATE TABLE IF NOT EXISTS rollout_artifacts (
 rollout TEXT PRIMARY KEY REFERENCES rollout_commands(id), plan BLOB NOT NULL,
 review BLOB NOT NULL, outputs BLOB, digest TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_cost_artifacts (
 rollout TEXT PRIMARY KEY REFERENCES rollout_commands(id), content BLOB NOT NULL, digest TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_approval_audit (
 rollout TEXT PRIMARY KEY REFERENCES rollout_commands(id), decision TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_plan_logs (
 rollout TEXT PRIMARY KEY REFERENCES rollout_commands(id), subject TEXT NOT NULL,
 content BLOB, too_large INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_pipeline_results (
 rollout TEXT PRIMARY KEY REFERENCES rollout_commands(id), body BLOB NOT NULL, outputs BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_execution_logs (
 rollout TEXT PRIMARY KEY REFERENCES rollout_commands(id), content BLOB NOT NULL, too_large INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_effects (
 rollout TEXT NOT NULL REFERENCES rollout_commands(id), step TEXT NOT NULL,
 status TEXT NOT NULL, PRIMARY KEY(rollout,step)
);
CREATE TABLE IF NOT EXISTS rollout_plan_uses (
 source_plan TEXT PRIMARY KEY REFERENCES rollout_commands(id),
 rollout TEXT NOT NULL UNIQUE REFERENCES rollout_commands(id)
);
CREATE TABLE IF NOT EXISTS rollout_decisions (
 id INTEGER PRIMARY KEY AUTOINCREMENT, rollout TEXT NOT NULL REFERENCES rollout_commands(id),
 kind TEXT NOT NULL, subject TEXT NOT NULL, actor TEXT NOT NULL, created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rollout_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, rollout TEXT NOT NULL REFERENCES rollout_commands(id),
 revision INTEGER NOT NULL, payload TEXT NOT NULL, acknowledged INTEGER NOT NULL DEFAULT 0,
 UNIQUE(rollout,revision)
);
CREATE TABLE IF NOT EXISTS rollout_deliveries (
 event_id INTEGER PRIMARY KEY REFERENCES rollout_events(id), content BLOB NOT NULL,
 result_owner TEXT UNIQUE REFERENCES rollout_commands(id)
);`

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func privateDirectory(directory string) (string, *os.File, error) {
	path, err := filepath.Abs(directory)
	if err != nil || directory == "" || path == string(filepath.Separator) {
		return "", nil, refused("invalid_directory")
	}
	if err = os.MkdirAll(path, 0o700); err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return "", nil, refused("invalid_directory")
	}
	if err = os.Chmod(path, 0o700); err != nil {
		return "", nil, err
	}
	lock, err := privateFile(filepath.Join(path, "coordinator.lock"))
	if err != nil {
		return "", nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return "", nil, refused("coordinator_busy")
	}
	return path, lock, nil
}

func privateFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, refused("invalid_storage_file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err != nil && f != nil {
		_ = f.Close()
	}
	return f, err
}

func siteIdentity(directory string) (*age.X25519Identity, error) {
	path := filepath.Join(directory, "identity.agekey")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		for _, name := range []string{"rollouts.sqlite", "rollouts.sqlite-wal", "rollouts.sqlite-shm"} {
			if _, statErr := os.Lstat(filepath.Join(directory, name)); !errors.Is(statErr, os.ErrNotExist) {
				return nil, refused("storage_identity_unavailable")
			}
		}
		return createSiteIdentity(directory)
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, refused("storage_identity_unavailable")
	}
	f, err := privateFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1024))
	if err != nil {
		return nil, err
	}
	identity, err := age.ParseX25519Identity(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, refused("storage_identity_unavailable")
	}
	return identity, nil
}

func createSiteIdentity(directory string) (*age.X25519Identity, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(directory, ".identity-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	if _, err = f.WriteString(identity.String() + "\n"); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = os.Rename(f.Name(), filepath.Join(directory, "identity.agekey")); err != nil {
		return nil, err
	}
	handle, err := os.Open(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	return identity, handle.Sync()
}

func (e *Engine) encrypt(data []byte) ([]byte, error) {
	var output bytes.Buffer
	w, err := age.Encrypt(&output, e.executor.Identity.Recipient())
	if err != nil {
		return nil, err
	}
	if _, err = w.Write(data); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (e *Engine) decrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	r, err := age.Decrypt(bytes.NewReader(data), e.executor.Identity)
	if err != nil {
		return nil, err
	}
	plain, err := io.ReadAll(io.LimitReader(r, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(plain) > 4<<20 {
		return nil, refused("artifact_too_large")
	}
	return plain, nil
}

func readProgress(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Progress, error) {
	var data []byte
	err := q.QueryRowContext(ctx, `SELECT progress FROM rollout_commands WHERE id=$1`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Progress{}, refused("not_found")
	}
	var p Progress
	if err == nil {
		err = json.Unmarshal(data, &p)
	}
	return p, err
}

func saveProgress(ctx context.Context, tx *sql.Tx, p *Progress) error {
	if p.InfrastructureApplied || p.Phase == "applying" || p.Phase == "outcome_unknown" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rollout_no_effect_blocks(rollout) VALUES($1) ON CONFLICT(rollout) DO NOTHING`, p.ID); err != nil {
			return err
		}
	}
	p.Revision++
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE rollout_commands SET progress=$1 WHERE id=$2`, data, p.ID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO rollout_events(rollout,revision,payload) VALUES($1,$2,$3)`, p.ID, p.Revision, data)
	return err
}

func (e *Engine) update(ctx context.Context, id string, fn func(*Progress) error) (Progress, error) {
	tx, err := e.beginCommand(ctx, id)
	if err != nil {
		return Progress{}, err
	}
	defer func() { _ = tx.Rollback() }()
	p, err := readProgress(ctx, tx, id)
	if err == nil {
		err = fn(&p)
	}
	if err == nil {
		err = saveProgress(ctx, tx, &p)
	}
	if err == nil {
		err = tx.Commit()
	}
	return p, err
}

func (e *Engine) command(ctx context.Context, id string) (Command, error) {
	return e.loadCommand(ctx, e.db, id)
}

// PendingEventHighWater bounds a volatile delivery pass even while new rows
// arrive. It does not transfer custody or acknowledge any row.
func (e *Engine) PendingEventHighWater(ctx context.Context) (int64, error) {
	var id int64
	err := e.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM rollout_events WHERE acknowledged=0`).Scan(&id)
	return id, err
}

func (e *Engine) Events(ctx context.Context, after int64) ([]Event, error) {
	rows, err := e.db.QueryContext(ctx, `SELECT id,payload FROM rollout_events WHERE id>$1 AND acknowledged=0 ORDER BY id LIMIT 100`, after)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]Event, 0)
	for rows.Next() {
		var item Event
		var data []byte
		if err = rows.Scan(&item.ID, &data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &item.Progress); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (e *Engine) AckEvent(ctx context.Context, id int64) error {
	tx, err := e.beginEvent(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE rollout_events SET acknowledged=1 WHERE id=$1`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return refused("event_unavailable")
	}
	return tx.Commit()
}

type PlanMetadata struct {
	ID          string `json:"id"`
	PlanDigest  string `json:"plan_digest"`
	PlanBytes   int64  `json:"plan_bytes"`
	ReviewBytes int64  `json:"review_bytes"`
	OutputBytes int64  `json:"output_bytes"`
}

func (e *Engine) GetPlanMetadata(ctx context.Context, id string) (PlanMetadata, error) {
	p := PlanMetadata{ID: id}
	err := e.db.QueryRowContext(ctx, `SELECT digest,length(plan),length(review),coalesce(length(outputs),0) FROM rollout_artifacts WHERE rollout=$1`, id).Scan(&p.PlanDigest, &p.PlanBytes, &p.ReviewBytes, &p.OutputBytes)
	return p, err
}
