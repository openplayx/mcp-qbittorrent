// Package torrents is a stateless application layer over the qBittorrent
// engine. Every read goes to the engine; nothing about tasks is stored by
// this service. What must survive between requests lives in the engine
// itself (tags for selection/hold intent) or in signed tokens the caller
// carries (plans, removal confirmations). Uploaded .torrent files are kept
// in memory only, briefly.
package torrents

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
)

// Engine is the subset of the qBittorrent adapter the service uses.
type Engine interface {
	Add(ctx context.Context, r qbittorrent.AddRequest) error
	Info(ctx context.Context, tag string) ([]qbittorrent.Torrent, error)
	InfoHash(ctx context.Context, hash string) ([]qbittorrent.Torrent, error)
	Files(ctx context.Context, hash string) ([]qbittorrent.File, error)
	SetFilePriority(ctx context.Context, hash string, indexes []int, priority int) error
	Stop(ctx context.Context, hash string) error
	Start(ctx context.Context, hash string) error
	Delete(ctx context.Context, hash string, deleteFiles bool) error
	SetLimits(ctx context.Context, hash string, download, upload int64) error
	AddTags(ctx context.Context, hash string, tags ...string) error
	RemoveTags(ctx context.Context, hash string, tags ...string) error
	Ping(ctx context.Context) error
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

// Engine tags carrying this service's intent. They are the only "state"
// and they live in the engine.
const (
	TagService     = "openplayx"         // added through this API (informational)
	TagPending     = "opx-pending"       // apply the initial selection once metadata is known
	TagAuto        = "opx-auto"          // start automatically after the initial selection
	TagPresetVideo = "opx-preset-video"  // initial selection: video files only
	TagHoldQuota   = "opx-hold-quota"    // stopped: selection exceeds the size quota
	TagHoldDisk    = "opx-hold-disk"     // stopped by the disk watermark; resumed automatically
	TagHoldStart   = "opx-hold-start"    // stopped: waiting for an explicit resume
	TagHoldNoMatch = "opx-hold-no-match" // stopped: the preset matched no file
)

type Service struct {
	cfg       *config.Config
	engine    Engine
	policy    Policy
	log       *slog.Logger
	aead      cipher.AEAD
	inputs    *inputStore
	freeBytes func(string) (int64, error)

	// Timings are fields so tests can shorten them.
	ConfirmTimeout time.Duration
	ConfirmPoll    time.Duration

	healthy  atomic.Bool
	lastSeen atomic.Int64
	locks    sync.Map // hash -> *sync.Mutex (serialises mutations per torrent)
}

func New(cfg *config.Config, engine Engine, log *slog.Logger, freeBytes func(string) (int64, error)) *Service {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	return &Service{
		cfg: cfg, engine: engine, log: log, aead: aead, freeBytes: freeBytes,
		policy:         Policy{AllowPrivate: cfg.Network.AllowPrivateNetworks, TrackerSchemes: cfg.Network.TrackerSchemes},
		inputs:         newInputStore(32),
		ConfirmTimeout: 15 * time.Second,
		ConfirmPoll:    300 * time.Millisecond,
	}
}

func (s *Service) SetPolicy(p Policy) { s.policy = p }

func (s *Service) lock(hash string) func() {
	m, _ := s.locks.LoadOrStore(hash, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- signed tokens (plans, removal confirmations) ----

// seal encrypts and authenticates a payload with a per-process key: tokens
// are opaque to callers and expire with the process.
func (s *Service) seal(prefix string, v any) string {
	raw, _ := json.Marshal(v)
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	out := s.aead.Seal(nonce, nonce, raw, []byte(prefix))
	return prefix + base64.RawURLEncoding.EncodeToString(out)
}

func (s *Service) unseal(prefix, token string, v any) bool {
	body, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return false
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(prefix))
	if err != nil {
		return false
	}
	return json.Unmarshal(plain, v) == nil
}

// ---- engine access helpers ----

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
			return fail(http.StatusNotFound, "task_not_found")
		case qbittorrent.CodeRejected:
			return fail(http.StatusConflict, "engine_rejected")
		default:
			return &Error{Status: http.StatusServiceUnavailable, Code: "engine_unavailable", Retryable: true}
		}
	}
	return &Error{Status: http.StatusInternalServerError, Code: "internal_error", Retryable: true}
}

// validID: engine hash ids are 40 lowercase hex (v1, or truncated v2).
func validID(id string) bool {
	if len(id) != 40 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

func (s *Service) get(ctx context.Context, id string) (qbittorrent.Torrent, error) {
	if !validID(id) {
		return qbittorrent.Torrent{}, fail(http.StatusNotFound, "task_not_found")
	}
	list, err := s.engine.InfoHash(ctx, id)
	if err != nil {
		return qbittorrent.Torrent{}, engineErr(err)
	}
	for _, t := range list {
		if t.Hash == id {
			return t, nil
		}
	}
	return qbittorrent.Torrent{}, fail(http.StatusNotFound, "task_not_found")
}

func tagSet(t qbittorrent.Torrent) map[string]bool {
	out := map[string]bool{}
	for _, tag := range strings.Split(t.Tags, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			out[tag] = true
		}
	}
	return out
}

// destinationOf maps an engine save path back to a destination ref.
func (s *Service) destinationOf(savePath string) (config.Destination, bool) {
	p := strings.TrimRight(savePath, "/")
	for _, d := range s.cfg.Destinations {
		dp := s.cfg.DestinationPath(d)
		if p == dp || strings.HasPrefix(p, dp+"/") {
			return d, true
		}
	}
	return config.Destination{}, false
}

func (s *Service) maxTaskBytes(savePath string) int64 {
	if d, ok := s.destinationOf(savePath); ok && d.MaxTaskBytes > 0 {
		return d.MaxTaskBytes
	}
	return s.cfg.Limits.MaxTaskBytes
}

// clampRate applies the administrator ceiling: 0 means "as fast as the
// ceiling allows"; above the ceiling is refused.
func clampRate(requested, ceiling int64) (int64, bool) {
	if requested < 0 {
		return 0, false
	}
	if ceiling > 0 && (requested == 0 || requested > ceiling) {
		return ceiling, requested <= ceiling
	}
	return requested, true
}

// waitFor polls the engine until cond holds for torrent id.
func (s *Service) waitFor(ctx context.Context, id string, cond func(t qbittorrent.Torrent, ok bool) bool) (qbittorrent.Torrent, error) {
	deadline := time.Now().Add(s.ConfirmTimeout)
	for {
		list, err := s.engine.InfoHash(ctx, id)
		if err == nil {
			var found qbittorrent.Torrent
			ok := false
			for _, t := range list {
				if t.Hash == id {
					found, ok = t, true
				}
			}
			if cond(found, ok) {
				return found, nil
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return qbittorrent.Torrent{}, &Error{Status: http.StatusGatewayTimeout, Code: "engine_state_unconfirmed", Retryable: true}
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.ConfirmPoll):
		}
	}
}

// ---- uploaded inputs (memory only) ----

type inputStore struct {
	mu    sync.Mutex
	max   int
	items map[string]inputItem
}

type inputItem struct {
	caller  string
	data    []byte
	expires time.Time
}

func newInputStore(max int) *inputStore {
	return &inputStore{max: max, items: map[string]inputItem{}}
}

func (st *inputStore) put(caller string, data []byte, ttl time.Duration) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for k, v := range st.items {
		if now.After(v.expires) {
			delete(st.items, k)
		}
	}
	for len(st.items) >= st.max {
		var oldest string
		for k, v := range st.items {
			if oldest == "" || v.expires.Before(st.items[oldest].expires) {
				oldest = k
			}
		}
		delete(st.items, oldest)
	}
	id := "inp_" + randomID(12)
	st.items[id] = inputItem{caller: caller, data: data, expires: now.Add(ttl)}
	return id
}

func (st *inputStore) get(caller, id string) ([]byte, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	v, ok := st.items[id]
	if !ok || v.caller != caller || time.Now().After(v.expires) {
		return nil, false
	}
	return v.data, true
}

func digest(parts ...any) string {
	raw, _ := json.Marshal(parts)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
