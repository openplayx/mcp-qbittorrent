package api

// MCP entry point (Streamable HTTP, POST /api/mcp).
//
// A thin protocol layer over the same torrents.Service, tokens, rate limit
// and request IDs as /api/v1: no second task store, no loopback HTTP.
//
// Supported subset: single JSON-RPC 2.0 messages over POST answered with
// application/json (no SSE, no server-initiated messages); stateless (no
// Mcp-Session-Id); initialize, ping, tools/list, tools/call and
// notifications. Business failures are tool results with isError=true and
// the same error codes as the HTTP API.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/openplayx/bittorrent-server/internal/torrents"
)

var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	maxMCPBody        = 64 << 10
)

const mcpInstructions = "Downloads run on the user's own server through an administrator-configured " +
	"engine and destinations. Downloading is two-step: create_plan validates the input and " +
	"returns a plan_ref plus what will happen; start_download submits that plan after the user " +
	"confirms. Removal is also two-step (create_remove_plan, then remove). Pausing never " +
	"deletes anything; removing a task and deleting its files are different choices."

type mcpTool struct {
	Name        string
	Title       string
	Description string
	Schema      map[string]any
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	Run         func(s *Server, r *http.Request, args json.RawMessage) (any, error)
}

func (t mcpTool) describe() map[string]any {
	return map[string]any{
		"name":        t.Name,
		"title":       t.Title,
		"description": t.Description,
		"inputSchema": t.Schema,
		"annotations": map[string]any{
			"title":           t.Title,
			"readOnlyHint":    t.ReadOnly,
			"destructiveHint": t.Destructive,
			"idempotentHint":  t.Idempotent,
			"openWorldHint":   false,
		},
	}
}

func object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var taskIDProp = map[string]any{"type": "string", "maxLength": 128, "description": "Task id from list_downloads or start_download."}

// decodeArgs is strict like the HTTP decoder: unknown fields are rejected.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		return &torrents.Error{Status: http.StatusBadRequest, Code: "invalid_request"}
	}
	return nil
}

type taskArgs struct {
	TaskID string `json:"task_id"`
}

func (a taskArgs) id() (string, error) {
	if a.TaskID == "" {
		return "", &torrents.Error{Status: http.StatusBadRequest, Code: "invalid_request", Detail: map[string]any{"field": "task_id"}}
	}
	return a.TaskID, nil
}

func taskTool(name, title, desc string, idempotent bool, run func(s *Server, r *http.Request, id string) (any, error)) mcpTool {
	return mcpTool{
		Name: name, Title: title, Description: desc, Idempotent: idempotent,
		Schema: object(map[string]any{"task_id": taskIDProp}, "task_id"),
		Run: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			var a taskArgs
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			id, err := a.id()
			if err != nil {
				return nil, err
			}
			return run(s, r, id)
		},
	}
}

var mcpTools = []mcpTool{
	{
		Name: "list_destinations", Title: "List destinations", ReadOnly: true, Idempotent: true,
		Description: "Download destinations the administrator allows, with free space.",
		Schema:      object(map[string]any{}),
		Run: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			if err := decodeArgs(raw, &struct{}{}); err != nil {
				return nil, err
			}
			return map[string]any{"destinations": s.svc.Destinations()}, nil
		},
	},
	{
		Name: "create_plan", Title: "Plan a download", ReadOnly: true, Idempotent: true,
		Description: "Validates a magnet (or an input_ref from the HTTP upload) against the " +
			"destination, size and duplicate rules and returns a short-lived plan_ref with what " +
			"would be downloaded. Nothing is added to the engine.",
		Schema: object(map[string]any{
			"magnet":           map[string]any{"type": "string", "maxLength": 8192, "description": "magnet:?xt=urn:btih:… link."},
			"input_ref":        map[string]any{"type": "string", "maxLength": 256, "description": "Reference returned by POST /api/v1/torrent-inputs."},
			"destination_ref":  map[string]any{"type": "string", "maxLength": 64, "description": "Ref from list_destinations."},
			"selection_preset": map[string]any{"type": "string", "enum": []string{"all", "video"}, "default": "all"},
			"auto_start":       map[string]any{"type": "boolean", "default": true},
		}, "destination_ref"),
		Run: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct {
				Magnet          string `json:"magnet"`
				InputRef        string `json:"input_ref"`
				DestinationRef  string `json:"destination_ref"`
				SelectionPreset string `json:"selection_preset"`
				AutoStart       *bool  `json:"auto_start"`
			}
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			src := torrents.PlanSource{Type: "magnet", Magnet: a.Magnet, InputRef: a.InputRef}
			if a.Magnet == "" && a.InputRef != "" {
				src.Type = "input"
			}
			return s.svc.CreatePlan(r.Context(), caller(r), torrents.PlanRequest{
				Source: src, DestinationRef: a.DestinationRef,
				SelectionPreset: a.SelectionPreset, AutoStart: a.AutoStart,
			})
		},
	},
	{
		Name: "start_download", Title: "Start download", Idempotent: true,
		Description: "Submits a plan_ref from create_plan (only after the user confirmed it). " +
			"Repeating it with the same plan returns the existing task instead of a duplicate.",
		Schema: object(map[string]any{
			"plan_ref": map[string]any{"type": "string", "maxLength": 4096, "description": "plan_ref from create_plan."},
		}, "plan_ref"),
		Run: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			var a torrents.CreateRequest
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			v, created, err := s.svc.CreateTask(r.Context(), caller(r), requestID(r), a)
			if err != nil {
				return nil, err
			}
			return map[string]any{"created": created, "task": v}, nil
		},
	},
	{
		Name: "list_downloads", Title: "List downloads", ReadOnly: true, Idempotent: true,
		Description: "Tasks under this service's download root with progress and state.",
		Schema:      object(map[string]any{}),
		Run: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			if err := decodeArgs(raw, &struct{}{}); err != nil {
				return nil, err
			}
			items, err := s.svc.ListTasks(r.Context())
			return map[string]any{"items": items}, err
		},
	},
	taskTool("get_download", "Download details", "One task's state, progress and rates.", true,
		func(s *Server, r *http.Request, id string) (any, error) { return s.svc.GetTask(r.Context(), id) }),
	taskTool("pause", "Pause download", "Pauses a task. Keeps all data.", true,
		func(s *Server, r *http.Request, id string) (any, error) {
			return s.svc.Pause(r.Context(), caller(r), requestID(r), id)
		}),
	taskTool("resume", "Resume download", "Resumes a paused task.", true,
		func(s *Server, r *http.Request, id string) (any, error) {
			return s.svc.Resume(r.Context(), caller(r), requestID(r), id)
		}),
	{
		Name: "create_remove_plan", Title: "Plan removal", ReadOnly: true, Idempotent: true,
		Description: "Describes what removing a task would do and returns a short-lived plan_ref " +
			"for remove. delete_files=true also deletes the downloaded data.",
		Schema: object(map[string]any{
			"task_id":      taskIDProp,
			"delete_files": map[string]any{"type": "boolean", "default": false},
		}, "task_id"),
		Run: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct {
				taskArgs
				DeleteFiles bool `json:"delete_files"`
			}
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			id, err := a.id()
			if err != nil {
				return nil, err
			}
			return s.svc.RemovePlan(r.Context(), id, torrents.RemovePlanRequest{DeleteFiles: a.DeleteFiles})
		},
	},
	{
		Name: "remove", Title: "Remove download", Destructive: true,
		Description: "Executes a plan_ref from create_remove_plan (only after the user confirmed it).",
		Schema: object(map[string]any{
			"task_id":  taskIDProp,
			"plan_ref": map[string]any{"type": "string", "maxLength": 4096},
		}, "task_id", "plan_ref"),
		Run: func(s *Server, r *http.Request, raw json.RawMessage) (any, error) {
			var a struct {
				taskArgs
				PlanRef string `json:"plan_ref"`
			}
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			id, err := a.id()
			if err != nil {
				return nil, err
			}
			return s.svc.Remove(r.Context(), caller(r), requestID(r), id, torrents.RemoveRequest{PlanRef: a.PlanRef})
		},
	},
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func rpcReply(w http.ResponseWriter, status int, id json.RawMessage, result any, rpcErr map[string]any) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if len(id) == 0 {
		body["id"] = nil
	}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	writeJSON(w, status, body)
}

func rpcError(code int, msg string) map[string]any {
	return map[string]any{"code": code, "message": msg}
}

func toolResult(payload any, isError bool) map[string]any {
	text, _ := json.Marshal(payload)
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": payload,
		"isError":           isError,
	}
}

func (s *Server) toolError(r *http.Request, err error) map[string]any {
	var e *torrents.Error
	if !errors.As(err, &e) {
		s.log.Error("internal error", "id", requestID(r), "err", err)
		e = &torrents.Error{Status: http.StatusInternalServerError, Code: "internal_error", Retryable: true}
	}
	body := map[string]any{"code": e.Code, "status": e.Status, "retryable": e.Retryable}
	if e.Detail != nil {
		body["detail"] = e.Detail
	}
	return toolResult(map[string]any{"error": body}, true)
}

// originAllowed is the DNS-rebinding guard: browsers may only call from our
// own origin; native clients send no Origin.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	_, host, _ := strings.Cut(o, "://")
	return strings.TrimSuffix(host, "/") == r.Host
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	if !originAllowed(r) {
		s.writeError(w, r, &torrents.Error{Status: http.StatusForbidden, Code: "origin_not_allowed"})
		return
	}
	if a := r.Header.Get("Accept"); a != "" && !strings.Contains(a, "application/json") && !strings.Contains(a, "*/*") {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcpVersions, v) {
		rpcReply(w, http.StatusBadRequest, nil, nil, rpcError(rpcInvalidRequest, "Unsupported protocol version."))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		rpcReply(w, http.StatusRequestEntityTooLarge, nil, nil, rpcError(rpcInvalidRequest, "Message too large."))
		return
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] != '{' {
		rpcReply(w, http.StatusBadRequest, nil, nil, rpcError(rpcInvalidRequest, "Expected one JSON-RPC 2.0 message."))
		return
	}
	var msg rpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		rpcReply(w, http.StatusBadRequest, nil, nil, rpcError(rpcParseError, "Parse error."))
		return
	}
	if msg.JSONRPC != "2.0" {
		rpcReply(w, http.StatusBadRequest, nil, nil, rpcError(rpcInvalidRequest, "Expected one JSON-RPC 2.0 message."))
		return
	}
	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		// Notifications and client responses: nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		version := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		rpcReply(w, http.StatusOK, msg.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "openplayx-bittorrent", "title": "OpenPlayX BitTorrent", "version": s.version},
			"instructions":    mcpInstructions,
		}, nil)
	case "ping":
		rpcReply(w, http.StatusOK, msg.ID, map[string]any{}, nil)
	case "tools/list":
		tools := make([]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, t.describe())
		}
		rpcReply(w, http.StatusOK, msg.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.Name == "" {
			rpcReply(w, http.StatusOK, msg.ID, nil, rpcError(rpcInvalidParams, "params.name is required."))
			return
		}
		i := slices.IndexFunc(mcpTools, func(t mcpTool) bool { return t.Name == p.Name })
		if i < 0 {
			rpcReply(w, http.StatusOK, msg.ID, nil, rpcError(rpcInvalidParams, "Unknown tool."))
			return
		}
		v, err := mcpTools[i].Run(s, r, p.Arguments)
		if err != nil {
			rpcReply(w, http.StatusOK, msg.ID, s.toolError(r, err), nil)
			return
		}
		rpcReply(w, http.StatusOK, msg.ID, toolResult(v, false), nil)
	default:
		rpcReply(w, http.StatusOK, msg.ID, nil, rpcError(rpcMethodNotFound, "Method not found."))
	}
}
