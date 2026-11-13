// Package storage persists the service's own view: API identities, caller
// ownership, plans, operations/idempotency results, engine state snapshots
// and an audit log. The engine stays the authority for download state.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS inputs (
  id TEXT PRIMARY KEY, caller TEXT NOT NULL, data BLOB NOT NULL,
  created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS plans (
  id TEXT PRIMARY KEY, caller TEXT NOT NULL, revision INTEGER NOT NULL,
  body TEXT NOT NULL, torrent BLOB, created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL, used_by TEXT
);
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY, caller TEXT NOT NULL, phase TEXT NOT NULL,
  v1 TEXT NOT NULL DEFAULT '', v2 TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL, snapshot TEXT, observed_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_caller ON tasks(caller, created_at);
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY, caller TEXT NOT NULL, kind TEXT NOT NULL,
  task_id TEXT NOT NULL DEFAULT '', idem_key TEXT NOT NULL,
  request_digest TEXT NOT NULL, status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '',
  retryable INTEGER NOT NULL DEFAULT 0, result TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  UNIQUE(caller, idem_key)
);
CREATE TABLE IF NOT EXISTS remove_plans (
  id TEXT PRIMARY KEY, caller TEXT NOT NULL, task_id TEXT NOT NULL,
  body TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
  used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, caller TEXT NOT NULL,
  request_id TEXT NOT NULL, action TEXT NOT NULL, task_id TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT ''
);
`

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dir, "bittorrent-server.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func now() int64 { return time.Now().Unix() }

// ---- inputs ----

func (s *Store) PutInput(ctx context.Context, id, caller string, data []byte, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO inputs(id, caller, data, created_at, expires_at) VALUES(?,?,?,?,?)`,
		id, caller, data, now(), now()+int64(ttl.Seconds()))
	return err
}

func (s *Store) GetInput(ctx context.Context, id, caller string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM inputs WHERE id=? AND caller=? AND expires_at>?`, id, caller, now()).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return data, err
}

// ---- plans ----

type Plan struct {
	ID        string
	Caller    string
	Revision  int
	Body      json.RawMessage
	Torrent   []byte
	CreatedAt int64
	ExpiresAt int64
	UsedBy    string
}

func (s *Store) PutPlan(ctx context.Context, p Plan) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO plans(id, caller, revision, body, torrent, created_at, expires_at) VALUES(?,?,?,?,?,?,?)`,
		p.ID, p.Caller, p.Revision, string(p.Body), p.Torrent, p.CreatedAt, p.ExpiresAt)
	return err
}

func (s *Store) GetPlan(ctx context.Context, id, caller string) (Plan, error) {
	var p Plan
	var body string
	var used sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, caller, revision, body, torrent, created_at, expires_at, used_by FROM plans WHERE id=? AND caller=?`, id, caller).
		Scan(&p.ID, &p.Caller, &p.Revision, &body, &p.Torrent, &p.CreatedAt, &p.ExpiresAt, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.Body, p.UsedBy = json.RawMessage(body), used.String
	return p, err
}

// ClaimPlan marks a plan as used by a task exactly once.
func (s *Store) ClaimPlan(ctx context.Context, id, taskID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE plans SET used_by=? WHERE id=? AND used_by IS NULL`, taskID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// ---- tasks ----

type Task struct {
	ID         string
	Caller     string
	Phase      string
	V1, V2     string
	Body       json.RawMessage
	Snapshot   json.RawMessage
	ObservedAt int64
	CreatedAt  int64
	UpdatedAt  int64
}

const taskCols = `id, caller, phase, v1, v2, body, COALESCE(snapshot, ''), observed_at, created_at, updated_at`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var body, snap string
	err := row.Scan(&t.ID, &t.Caller, &t.Phase, &t.V1, &t.V2, &body, &snap, &t.ObservedAt, &t.CreatedAt, &t.UpdatedAt)
	t.Body = json.RawMessage(body)
	if snap != "" {
		t.Snapshot = json.RawMessage(snap)
	}
	return t, err
}

func (s *Store) PutTask(ctx context.Context, t Task) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id, caller, phase, v1, v2, body, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, t.Caller, t.Phase, t.V1, t.V2, string(t.Body), now(), now())
	return err
}

func (s *Store) UpdateTask(ctx context.Context, id, phase string, body json.RawMessage) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET phase=?, body=?, updated_at=? WHERE id=?`, phase, string(body), now(), id)
	return err
}

func (s *Store) UpdateSnapshot(ctx context.Context, id string, snap json.RawMessage, observed int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET snapshot=?, observed_at=? WHERE id=?`, string(snap), observed, id)
	return err
}

func (s *Store) GetTask(ctx context.Context, id, caller string) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=? AND caller=?`, id, caller))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Store) GetTaskAny(ctx context.Context, id string) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// ListTasks pages by (created_at, id) descending. cursor is the last id seen.
func (s *Store) ListTasks(ctx context.Context, caller string, includeRemoved bool, cursor string, limit int) ([]Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks WHERE caller=?`
	args := []any{caller}
	if !includeRemoved {
		q += ` AND phase != 'removed'`
	}
	if cursor != "" {
		var at int64
		if err := s.db.QueryRowContext(ctx, `SELECT created_at FROM tasks WHERE id=? AND caller=?`, cursor, caller).Scan(&at); err != nil {
			return nil, ErrNotFound
		}
		q += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, at, at, cursor)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ActiveTasks returns every task the reconciler must observe.
func (s *Store) ActiveTasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE phase NOT IN ('removed','failed') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FindLiveTaskByIdentity finds a non-removed task with the same payload.
func (s *Store) FindLiveTaskByIdentity(ctx context.Context, v1, v2 string) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE phase NOT IN ('removed','failed') AND ((?!='' AND v1=?) OR (?!='' AND v2=?)) LIMIT 1`, v1, v1, v2, v2))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// ---- operations ----

type Operation struct {
	ID            string          `json:"id"`
	Caller        string          `json:"-"`
	Kind          string          `json:"kind"`
	TaskID        string          `json:"task_id,omitempty"`
	IdemKey       string          `json:"-"`
	RequestDigest string          `json:"-"`
	Status        string          `json:"status"` // pending | succeeded | failed
	Error         string          `json:"error_code,omitempty"`
	Retryable     bool            `json:"retryable"`
	Result        json.RawMessage `json:"result,omitempty"`
	CreatedAt     int64           `json:"created_at"`
	UpdatedAt     int64           `json:"updated_at"`
}

const opCols = `id, caller, kind, task_id, idem_key, request_digest, status, error, retryable, result, created_at, updated_at`

func scanOp(row interface{ Scan(...any) error }) (Operation, error) {
	var o Operation
	var result string
	err := row.Scan(&o.ID, &o.Caller, &o.Kind, &o.TaskID, &o.IdemKey, &o.RequestDigest, &o.Status, &o.Error, &o.Retryable, &result, &o.CreatedAt, &o.UpdatedAt)
	o.Result = json.RawMessage(result)
	return o, err
}

// BeginOperation inserts a pending operation or returns the existing one
// for the same (caller, key). existing=true means the caller must not redo
// the work; a different request digest yields ErrConflict.
func (s *Store) BeginOperation(ctx context.Context, o Operation) (Operation, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return o, false, err
	}
	defer tx.Rollback()
	prev, err := scanOp(tx.QueryRowContext(ctx, `SELECT `+opCols+` FROM operations WHERE caller=? AND idem_key=?`, o.Caller, o.IdemKey))
	if err == nil {
		if prev.RequestDigest != o.RequestDigest {
			return prev, true, ErrConflict
		}
		return prev, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return o, false, err
	}
	o.Status, o.CreatedAt, o.UpdatedAt = "pending", now(), now()
	if o.Result == nil {
		o.Result = json.RawMessage(`{}`)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations(`+opCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.Caller, o.Kind, o.TaskID, o.IdemKey, o.RequestDigest, o.Status, o.Error, o.Retryable, string(o.Result), o.CreatedAt, o.UpdatedAt); err != nil {
		return o, false, err
	}
	return o, false, tx.Commit()
}

func (s *Store) FinishOperation(ctx context.Context, id, status, errCode string, retryable bool, result any) error {
	raw := []byte(`{}`)
	if result != nil {
		var err error
		if raw, err = json.Marshal(result); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET status=?, error=?, retryable=?, result=?, updated_at=? WHERE id=?`,
		status, errCode, retryable, string(raw), now(), id)
	return err
}

func (s *Store) SetOperationTask(ctx context.Context, id, taskID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET task_id=? WHERE id=?`, taskID, id)
	return err
}

func (s *Store) GetOperation(ctx context.Context, id, caller string) (Operation, error) {
	o, err := scanOp(s.db.QueryRowContext(ctx, `SELECT `+opCols+` FROM operations WHERE id=? AND caller=?`, id, caller))
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}

// PendingOperations lets startup resolve operations interrupted by a crash.
func (s *Store) PendingOperations(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+opCols+` FROM operations WHERE status='pending'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---- remove plans ----

func (s *Store) PutRemovePlan(ctx context.Context, id, caller, taskID string, body json.RawMessage, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO remove_plans(id, caller, task_id, body, created_at, expires_at) VALUES(?,?,?,?,?,?)`,
		id, caller, taskID, string(body), now(), now()+int64(ttl.Seconds()))
	return err
}

func (s *Store) GetRemovePlan(ctx context.Context, id, caller string) (taskID string, body json.RawMessage, expired, used bool, err error) {
	var b string
	var exp int64
	err = s.db.QueryRowContext(ctx, `SELECT task_id, body, expires_at, used FROM remove_plans WHERE id=? AND caller=?`, id, caller).Scan(&taskID, &b, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, false, ErrNotFound
	}
	return taskID, json.RawMessage(b), exp <= now(), used, err
}

func (s *Store) UseRemovePlan(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE remove_plans SET used=1 WHERE id=? AND used=0`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// ---- audit & retention ----

func (s *Store) Audit(ctx context.Context, caller, requestID, action, taskID, detail string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit(at, caller, request_id, action, task_id, detail) VALUES(?,?,?,?,?,?)`,
		now(), caller, requestID, action, taskID, detail)
}

// Prune drops expired inputs/plans and old finished operations/audit rows.
func (s *Store) Prune(ctx context.Context, retention time.Duration) error {
	cut := now() - int64(retention.Seconds())
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM inputs WHERE expires_at <= ?`, []any{now()}},
		{`DELETE FROM plans WHERE expires_at <= ? AND used_by IS NULL`, []any{now()}},
		{`DELETE FROM plans WHERE created_at <= ?`, []any{cut}},
		{`DELETE FROM remove_plans WHERE expires_at <= ?`, []any{cut}},
		{`DELETE FROM operations WHERE status != 'pending' AND updated_at <= ?`, []any{cut}},
		{`DELETE FROM audit WHERE at <= ?`, []any{cut}},
	} {
		if _, err := s.db.ExecContext(ctx, q.sql, q.args...); err != nil {
			return err
		}
	}
	return nil
}
