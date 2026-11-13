// Package torrents is the application service: plans, ownership, creation,
// control operations and state projection over the engine adapter.
package torrents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openplayx/bittorrent-server/internal/config"
	"github.com/openplayx/bittorrent-server/internal/qbittorrent"
	"github.com/openplayx/bittorrent-server/internal/storage"
)

// Engine is the subset of the qBittorrent adapter the service uses; tests
// substitute the fake engine through the real HTTP adapter.
type Engine interface {
	Add(ctx context.Context, r qbittorrent.AddRequest) error
	Info(ctx context.Context, tag string) ([]qbittorrent.Torrent, error)
	Files(ctx context.Context, hash string) ([]qbittorrent.File, error)
	SetFilePriority(ctx context.Context, hash string, indexes []int, priority int) error
	Stop(ctx context.Context, hash string) error
	Start(ctx context.Context, hash string) error
	Delete(ctx context.Context, hash string) error
	SetLimits(ctx context.Context, hash string, download, upload int64) error
	Versions() (string, string)
}

// Error is a business error with a stable code and HTTP status.
type Error struct {
	Status    int
	Code      string
	Retryable bool
	Detail    map[string]any
}

func (e *Error) Error() string { return e.Code }

func fail(status int, code string) *Error { return &Error{Status: status, Code: code} }

// ServiceTag marks every engine torrent this service created; it only aids
// tracing, ownership is decided by the database.
const ServiceTag = "openplayx"

type Service struct {
	cfg      *config.Config
	store    *storage.Store
	engine   Engine
	policy   Policy
	log      *slog.Logger
	instance string

	// Timings are fields so tests can shorten them.
	ConfirmTimeout time.Duration
	ConfirmPoll    time.Duration

	healthy   atomic.Bool
	lastSeen  atomic.Int64
	taskLocks sync.Map // task id -> *sync.Mutex
	freeBytes func(string) (int64, error)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(cfg *config.Config, store *storage.Store, engine Engine, log *slog.Logger, freeBytes func(string) (int64, error)) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	h := sha256.Sum256([]byte(cfg.Engine.URL))
	return &Service{
		cfg:    cfg,
		store:  store,
		engine: engine,
		policy: Policy{AllowPrivate: cfg.Network.AllowPrivateNetworks, TrackerSchemes: cfg.Network.TrackerSchemes},
		log:    log,
		// Engine instance identity is recorded on every task so a future
		// multi-engine deployment never confuses tasks across engines.
		instance:       "qbt-" + hex.EncodeToString(h[:6]),
		ConfirmTimeout: 30 * time.Second,
		ConfirmPoll:    500 * time.Millisecond,
		freeBytes:      freeBytes,
		ctx:            ctx,
		cancel:         cancel,
	}
}

func (s *Service) SetPolicy(p Policy) { s.policy = p }

func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Service) go_(f func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f(s.ctx)
	}()
}

func (s *Service) lock(taskID string) func() {
	m, _ := s.taskLocks.LoadOrStore(taskID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Service) EngineHealthy() bool { return s.healthy.Load() }

func newID(prefix string) string {
	b := make([]byte, 15)
	_, _ = rand.Read(b)
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func digest(parts ...any) string {
	raw, _ := json.Marshal(parts)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// engineErr maps adapter failures to API errors without leaking details.
func engineErr(err error) *Error {
	var be *Error
	if errors.As(err, &be) {
		return be
	}
	var qe *qbittorrent.Error
	if errors.As(err, &qe) {
		switch qe.Code {
		case qbittorrent.CodeNotFound:
			return &Error{Status: http.StatusConflict, Code: "engine_task_missing"}
		case qbittorrent.CodeRejected:
			return &Error{Status: http.StatusConflict, Code: "engine_rejected"}
		default:
			return &Error{Status: http.StatusServiceUnavailable, Code: "engine_unavailable", Retryable: true}
		}
	}
	return &Error{Status: http.StatusInternalServerError, Code: "internal_error", Retryable: true}
}

// TaskBody is the service's own record of a task (JSON column).
type TaskBody struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	InputKind       string `json:"input_kind"`
	Destination     string `json:"destination"`
	SavePath        string `json:"save_path"`
	Tag             string `json:"tag"`
	EngineInstance  string `json:"engine_instance"`
	EngineHash      string `json:"engine_hash,omitempty"`
	SelectionPreset string `json:"selection_preset"`
	AutoStart       bool   `json:"auto_start"`
	DownloadLimit   int64  `json:"download_limit"`
	UploadLimit     int64  `json:"upload_limit"`
	PlanID          string `json:"plan_id"`
	CreateOp        string `json:"create_op"`
	HoldReason      string `json:"hold_reason,omitempty"`
	UserPaused      bool   `json:"user_paused"`
	DiskPaused      bool   `json:"disk_paused"`
	SelectionDone   bool   `json:"selection_done"`
	Started         bool   `json:"started"`
	FilesRevision   int    `json:"files_revision"`
	Revision        int    `json:"revision"`
	Error           string `json:"error,omitempty"`
	RemovedAt       int64  `json:"removed_at,omitempty"`
	DataDeleted     bool   `json:"data_deleted,omitempty"`
	MissingSince    int64  `json:"missing_since,omitempty"`
}

// Snapshot is the last observed engine projection (JSON column).
type Snapshot struct {
	EngineState string         `json:"engine_state"`
	HasMetadata bool           `json:"has_metadata"`
	Progress    float64        `json:"progress"`
	Size        int64          `json:"size"`
	TotalSize   int64          `json:"total_size"`
	Completed   int64          `json:"completed"`
	Downloaded  int64          `json:"downloaded"`
	Uploaded    int64          `json:"uploaded"`
	DlSpeed     int64          `json:"dlspeed"`
	UpSpeed     int64          `json:"upspeed"`
	DlLimit     int64          `json:"dl_limit"`
	UpLimit     int64          `json:"up_limit"`
	Seeds       int            `json:"seeds"`
	Peers       int            `json:"peers"`
	Ratio       float64        `json:"ratio"`
	ETA         int64          `json:"eta"`
	Missing     bool           `json:"missing"`
	Files       []SnapshotFile `json:"files,omitempty"`
}

type SnapshotFile struct {
	Index    int     `json:"index"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	Priority int     `json:"priority"`
}

type taskRec struct {
	row  storage.Task
	body TaskBody
	snap *Snapshot
}

func decodeTask(t storage.Task) (*taskRec, error) {
	r := &taskRec{row: t}
	if err := json.Unmarshal(t.Body, &r.body); err != nil {
		return nil, err
	}
	if len(t.Snapshot) > 0 {
		r.snap = &Snapshot{}
		if err := json.Unmarshal(t.Snapshot, r.snap); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (s *Service) loadTask(ctx context.Context, id, caller string) (*taskRec, error) {
	t, err := s.store.GetTask(ctx, id, caller)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, fail(http.StatusNotFound, "task_not_found")
	}
	if err != nil {
		return nil, err
	}
	return decodeTask(t)
}

func (s *Service) saveTask(ctx context.Context, r *taskRec, phase string) error {
	r.body.Revision++
	raw, err := json.Marshal(r.body)
	if err != nil {
		return err
	}
	r.row.Phase = phase
	return s.store.UpdateTask(ctx, r.row.ID, phase, raw)
}

func (s *Service) saveSnapshot(ctx context.Context, r *taskRec, snap *Snapshot) error {
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	r.snap = snap
	r.row.ObservedAt = time.Now().Unix()
	return s.store.UpdateSnapshot(ctx, r.row.ID, raw, r.row.ObservedAt)
}

func (s *Service) maxTaskBytes(dest string) int64 {
	if d, ok := s.cfg.Destination(dest); ok && d.MaxTaskBytes > 0 {
		return d.MaxTaskBytes
	}
	return s.cfg.Limits.MaxTaskBytes
}

// clampRate applies the administrator ceiling: 0 means "as fast as the
// ceiling allows".
func clampRate(requested, ceiling int64) (int64, bool) {
	if requested < 0 {
		return 0, false
	}
	if ceiling > 0 && (requested == 0 || requested > ceiling) {
		return ceiling, requested <= ceiling
	}
	return requested, true
}
