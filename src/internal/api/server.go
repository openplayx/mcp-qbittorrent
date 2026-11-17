// Package api is the authenticated HTTP surface over the torrent service.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	apispec "github.com/openplayx/bittorrent-server/src/api"
	"github.com/openplayx/bittorrent-server/src/internal/config"
	"github.com/openplayx/bittorrent-server/src/internal/torrents"
	"github.com/openplayx/bittorrent-server/src/internal/webui"
)

type Server struct {
	svc     *torrents.Service
	cfg     *config.Config
	tokens  []config.APIToken
	log     *slog.Logger
	version string
	mux     *http.ServeMux
	limiter *limiter
	webui   *webui.Proxy
	// Routes lists registered patterns (for the OpenAPI contract test).
	Routes []string
}

type ctxKey int

const (
	keyCaller ctxKey = iota
	keyRequestID
)

func New(svc *torrents.Service, cfg *config.Config, tokens []config.APIToken, log *slog.Logger, version string) *Server {
	s := &Server{
		svc: svc, cfg: cfg, tokens: tokens, log: log, version: version,
		mux: http.NewServeMux(), limiter: newLimiter(cfg.Limits.RequestsPerMinute),
	}
	s.routes()
	if cfg.WebUI.Enabled {
		reserved := []string{}
		for _, d := range cfg.Destinations {
			reserved = append(reserved, cfg.DestinationPath(d))
		}
		p, err := webui.New(cfg.Engine.URL, cfg.DownloadRoot, reserved, cfg.WebUI.LoginFailuresPerClient, log)
		if err == nil {
			s.webui = p
		}
	}
	return s
}

// isAPI: everything under /api/ belongs to this service except /api/v2/,
// which is the engine WebUI's own API.
func isAPI(p string) bool {
	return strings.HasPrefix(p, "/api/") && p != "/api/v2" && !strings.HasPrefix(p, "/api/v2/")
}

func (s *Server) handle(pattern string, authed bool, h http.HandlerFunc) {
	s.Routes = append(s.Routes, pattern)
	if authed {
		h = s.auth(h)
	}
	s.mux.HandleFunc(pattern, h)
}

func (s *Server) routes() {
	s.handle("GET /api/healthz", false, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.version})
	})
	s.handle("GET /api/readyz", false, s.readyz)
	s.handle("GET /api/openapi.json", false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(apispec.OpenAPI)
	})
	s.handle("GET /api/docs", false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline' 'self'")
		_, _ = w.Write(apispec.DocsHTML)
	})
	s.handle("GET /api/v1/capabilities", true, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.svc.Capabilities(r.Context(), s.version))
	})
	s.handle("GET /api/v1/destinations", true, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"destinations": s.svc.Destinations()})
	})
	s.handle("POST /api/v1/torrent-inputs", true, s.createInput)
	s.handle("POST /api/v1/torrent-plans", true, func(w http.ResponseWriter, r *http.Request) {
		var req torrents.PlanRequest
		if !s.decode(w, r, &req) {
			return
		}
		v, err := s.svc.CreatePlan(r.Context(), caller(r), req)
		s.reply(w, r, http.StatusCreated, v, err)
	})
	s.handle("POST /api/v1/torrents", true, func(w http.ResponseWriter, r *http.Request) {
		var req torrents.CreateRequest
		if !s.decode(w, r, &req) {
			return
		}
		v, created, err := s.svc.CreateTask(r.Context(), caller(r), requestID(r), req)
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		s.reply(w, r, status, v, err)
	})
	s.handle("GET /api/v1/torrents", true, func(w http.ResponseWriter, r *http.Request) {
		items, err := s.svc.ListTasks(r.Context())
		s.reply(w, r, http.StatusOK, map[string]any{"items": items}, err)
	})
	s.handle("GET /api/v1/torrents/{id}", true, func(w http.ResponseWriter, r *http.Request) {
		v, err := s.svc.GetTask(r.Context(), r.PathValue("id"))
		s.reply(w, r, http.StatusOK, v, err)
	})
	s.handle("GET /api/v1/torrents/{id}/files", true, func(w http.ResponseWriter, r *http.Request) {
		v, err := s.svc.Files(r.Context(), r.PathValue("id"))
		s.reply(w, r, http.StatusOK, v, err)
	})
	s.handle("PATCH /api/v1/torrents/{id}/selection", true, func(w http.ResponseWriter, r *http.Request) {
		var req torrents.SelectionRequest
		if !s.decode(w, r, &req) {
			return
		}
		v, err := s.svc.SetSelection(r.Context(), caller(r), requestID(r), r.PathValue("id"), req)
		s.reply(w, r, http.StatusOK, v, err)
	})
	s.handle("POST /api/v1/torrents/{id}/pause", true, func(w http.ResponseWriter, r *http.Request) {
		v, err := s.svc.Pause(r.Context(), caller(r), requestID(r), r.PathValue("id"))
		s.reply(w, r, http.StatusOK, v, err)
	})
	s.handle("POST /api/v1/torrents/{id}/resume", true, func(w http.ResponseWriter, r *http.Request) {
		v, err := s.svc.Resume(r.Context(), caller(r), requestID(r), r.PathValue("id"))
		s.reply(w, r, http.StatusOK, v, err)
	})
	s.handle("PATCH /api/v1/torrents/{id}/limits", true, func(w http.ResponseWriter, r *http.Request) {
		var req torrents.LimitsRequest
		if !s.decode(w, r, &req) {
			return
		}
		v, err := s.svc.SetLimits(r.Context(), caller(r), requestID(r), r.PathValue("id"), req)
		s.reply(w, r, http.StatusOK, v, err)
	})
	s.handle("POST /api/v1/torrents/{id}/remove-plan", true, func(w http.ResponseWriter, r *http.Request) {
		var req torrents.RemovePlanRequest
		if !s.decodeOptional(w, r, &req) {
			return
		}
		v, err := s.svc.RemovePlan(r.Context(), r.PathValue("id"), req)
		s.reply(w, r, http.StatusCreated, v, err)
	})
	s.handle("POST /api/v1/torrents/{id}/remove", true, func(w http.ResponseWriter, r *http.Request) {
		var req torrents.RemoveRequest
		if !s.decode(w, r, &req) {
			return
		}
		v, err := s.svc.Remove(r.Context(), caller(r), requestID(r), r.PathValue("id"), req)
		s.reply(w, r, http.StatusOK, v, err)
	})
	s.handle("GET /api/v1/torrents/{id}/files/{file_id}/content", true, s.content)
	// MCP is JSON-RPC, not a REST resource, so it stays out of Routes (the
	// OpenAPI contract); other methods on the path get 405 from the mux.
	s.mux.HandleFunc("POST /api/mcp", s.auth(s.mcp))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rid := r.Header.Get("X-Request-ID")
	if len(rid) == 0 || len(rid) > 64 || strings.ContainsAny(rid, "\r\n ") {
		rid = newRequestID()
	}
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	route := "webui"
	switch {
	case isAPI(r.URL.Path):
		w.Header().Set("X-Request-ID", rid)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		ctx := context.WithValue(r.Context(), keyRequestID, rid)
		s.mux.ServeHTTP(sw, r.WithContext(ctx))
		_, route = s.mux.Handler(r)
	case s.webui != nil:
		if e, ok := strings.CutPrefix(r.URL.Path, "/api/v2/"); ok {
			route = "webui /api/v2/" + e
		}
		s.webui.ServeHTTP(sw, r)
	case r.URL.Path == "/":
		http.Redirect(sw, r, "/api/docs", http.StatusFound)
	default:
		http.NotFound(sw, r)
	}
	// Paths only; never query strings or bodies (magnets, tokens).
	s.log.Info("request", "id", rid, "method", r.Method, "route", route, "status", sw.status, "ms", time.Since(start).Milliseconds())
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func newRequestID() string {
	return "req_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func caller(r *http.Request) string    { c, _ := r.Context().Value(keyCaller).(string); return c }
func requestID(r *http.Request) string { c, _ := r.Context().Value(keyRequestID).(string); return c }

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		who := ""
		if ok && token != "" {
			sum := config.HashToken(token)
			for _, t := range s.tokens {
				if subtle.ConstantTimeCompare([]byte(sum), []byte(t.TokenSHA256)) == 1 {
					who = t.Caller
				}
			}
		}
		if who == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bittorrent-server"`)
			s.writeError(w, r, &torrents.Error{Status: http.StatusUnauthorized, Code: "unauthorized"})
			return
		}
		if wait, ok := s.limiter.allow(who); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			s.writeError(w, r, &torrents.Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Retryable: true})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), keyCaller, who)))
	}
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	h := s.svc.Health(r.Context())
	status := http.StatusOK
	if !h.Engine {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": h.Engine, "engine": h.Engine})
}

const maxJSONBody = 64 << 10

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		s.writeError(w, r, &torrents.Error{Status: http.StatusUnsupportedMediaType, Code: "json_required"})
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.writeError(w, r, &torrents.Error{Status: http.StatusBadRequest, Code: "invalid_request"})
		return false
	}
	if dec.More() {
		s.writeError(w, r, &torrents.Error{Status: http.StatusBadRequest, Code: "invalid_request"})
		return false
	}
	return true
}

// decodeOptional accepts an empty body for parameterless controls.
func (s *Server) decodeOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 && r.Header.Get("Content-Type") == "" {
		return true
	}
	return s.decode(w, r, v)
}

func (s *Server) reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, status, v)
}

func (s *Server) createInput(w http.ResponseWriter, r *http.Request) {
	max := s.cfg.Limits.MaxTorrentFileBytes
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var data []byte
	var err error
	switch mt {
	case "application/x-bittorrent", "application/octet-stream":
		data, err = io.ReadAll(http.MaxBytesReader(w, r.Body, max+1))
	case "multipart/form-data":
		r.Body = http.MaxBytesReader(w, r.Body, max+64<<10)
		if err = r.ParseMultipartForm(max + 64<<10); err == nil {
			f, _, ferr := r.FormFile("torrent")
			if ferr != nil {
				err = ferr
			} else {
				data, err = io.ReadAll(io.LimitReader(f, max+1))
				f.Close()
			}
		}
	default:
		s.writeError(w, r, &torrents.Error{Status: http.StatusUnsupportedMediaType, Code: "torrent_content_type_required"})
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || int64(len(data)) > max {
		s.writeError(w, r, &torrents.Error{Status: http.StatusRequestEntityTooLarge, Code: "torrent_too_large"})
		return
	}
	if err != nil || len(data) == 0 {
		s.writeError(w, r, &torrents.Error{Status: http.StatusBadRequest, Code: "invalid_request"})
		return
	}
	v, err := s.svc.CreateInput(r.Context(), caller(r), data)
	s.reply(w, r, http.StatusCreated, v, err)
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	f, info, err := s.svc.OpenContent(r.Context(), r.PathValue("id"), r.PathValue("file_id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer f.Close()
	if ct := mime.TypeByExtension(strings.ToLower(path.Ext(info.Name()))); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "private, max-age=0")
	// ServeContent implements HEAD, Range, If-Range and conditional GETs.
	http.ServeContent(w, r, "", info.ModTime(), f)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var e *torrents.Error
	if !errors.As(err, &e) {
		s.log.Error("internal error", "id", requestID(r), "err", err)
		e = &torrents.Error{Status: http.StatusInternalServerError, Code: "internal_error", Retryable: true}
	}
	body := map[string]any{"code": e.Code, "request_id": requestID(r), "retryable": e.Retryable}
	if e.Detail != nil {
		body["detail"] = e.Detail
	}
	writeJSON(w, e.Status, map[string]any{"error": body})
}

// limiter is a per-caller fixed one-minute window.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window map[string]*bucket
}

type bucket struct {
	start time.Time
	count int
}

func newLimiter(perMinute int) *limiter {
	return &limiter{limit: perMinute, window: map[string]*bucket{}}
}

func (l *limiter) allow(who string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.window[who]
	now := time.Now()
	if b == nil || now.Sub(b.start) >= time.Minute {
		l.window[who] = &bucket{start: now, count: 1}
		return 0, true
	}
	if b.count >= l.limit {
		return time.Minute - now.Sub(b.start), false
	}
	b.count++
	return 0, true
}
