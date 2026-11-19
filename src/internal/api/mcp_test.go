package api_test

import (
	"encoding/json"
	"testing"
)

// rpc posts one JSON-RPC message to /api/mcp with the shared bearer token.
func (h *harness) rpc(method string, params any, hdr ...string) resp {
	h.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		msg["params"] = params
	}
	return h.do("POST", "/api/mcp", msg, append([]string{"Accept", "application/json, text/event-stream"}, hdr...)...)
}

type toolOut struct {
	IsError    bool           `json:"isError"`
	Structured map[string]any `json:"structuredContent"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (h *harness) tool(name string, args map[string]any) toolOut {
	h.t.Helper()
	r := h.expect(h.rpc("tools/call", map[string]any{"name": name, "arguments": args}), 200)
	var out struct {
		Result *toolOut        `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(r.raw, &out); err != nil || out.Result == nil {
		h.t.Fatalf("%s: %s", name, r.raw)
	}
	if len(out.Result.Content) != 1 || out.Result.Content[0].Type != "text" {
		h.t.Fatalf("%s content: %s", name, r.raw)
	}
	return *out.Result
}

func (o toolOut) code() string {
	e, _ := o.Structured["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestMCPHandshakeAndCatalog(t *testing.T) {
	h := newHarness(t)
	init := h.expect(h.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}), 200)
	res := init.body["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "openplayx-bittorrent" {
		t.Fatal(string(init.raw))
	}
	if init.header.Get("Mcp-Session-Id") != "" {
		t.Fatal("stateless server must not issue a session")
	}
	if r := h.rpc("initialize", map[string]any{"protocolVersion": "1999-01-01"}); r.body["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Fatal(string(r.raw))
	}
	if r := h.do("POST", "/api/mcp", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); r.status != 202 || len(r.raw) != 0 {
		t.Fatalf("notification: %d %s", r.status, r.raw)
	}
	list := h.expect(h.rpc("tools/list", nil), 200)
	names := map[string]map[string]any{}
	for _, raw := range list.body["result"].(map[string]any)["tools"].([]any) {
		tool := raw.(map[string]any)
		names[tool["name"].(string)] = tool
	}
	for _, n := range []string{"list_destinations", "create_plan", "start_download", "list_downloads", "get_download", "pause", "resume", "create_remove_plan", "remove"} {
		if names[n] == nil {
			t.Errorf("missing tool %s", n)
		}
	}
	for _, n := range []string{"list_destinations", "create_plan", "list_downloads", "get_download", "create_remove_plan"} {
		if names[n]["annotations"].(map[string]any)["readOnlyHint"] != true {
			t.Errorf("%s must be marked read-only", n)
		}
	}
	for _, n := range []string{"start_download", "pause", "resume", "remove"} {
		if names[n]["annotations"].(map[string]any)["readOnlyHint"] != false {
			t.Errorf("%s must remain a write requiring confirmation", n)
		}
	}
	if names["remove"]["annotations"].(map[string]any)["destructiveHint"] != true {
		t.Error("remove must be marked destructive")
	}
	if names["pause"]["annotations"].(map[string]any)["destructiveHint"] != false {
		t.Error("pause must not be marked destructive")
	}
}

func TestMCPProtocolGuards(t *testing.T) {
	h := newHarness(t)
	if r := h.rpc("ping", nil, "Authorization", ""); r.status != 401 || r.header.Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthenticated: %d", r.status)
	}
	if r := h.rpc("ping", nil, "Origin", "https://evil.example"); r.status != 403 {
		t.Fatalf("foreign origin: %d", r.status)
	}
	if r := h.rpc("ping", nil, "Accept", "text/html"); r.status != 406 {
		t.Fatalf("accept: %d", r.status)
	}
	if r := h.rpc("ping", nil, "MCP-Protocol-Version", "1999-01-01"); r.status != 400 {
		t.Fatalf("version: %d", r.status)
	}
	if r := h.do("GET", "/api/mcp", nil); r.status != 405 {
		t.Fatalf("GET: %d", r.status)
	}
	if r := h.do("POST", "/api/mcp", []any{map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}}); r.status != 400 {
		t.Fatalf("batch: %d", r.status)
	}
	if r := h.rpc("resources/list", nil); r.body["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatal(string(r.raw))
	}
	if r := h.rpc("tools/call", map[string]any{"name": "nope"}); r.body["error"].(map[string]any)["code"].(float64) != -32602 {
		t.Fatal(string(r.raw))
	}
}

func TestMCPDownloadLifecycle(t *testing.T) {
	h := newHarness(t)
	if d := h.tool("list_destinations", nil); d.IsError || len(d.Structured["destinations"].([]any)) != 2 {
		t.Fatalf("destinations: %+v", d)
	}
	// Files go through the HTTP upload; the tool only receives its reference.
	in := h.expect(h.do("POST", "/api/v1/torrent-inputs", makeTorrent("Show", map[string]int{"e1.mkv": 400, "notes.txt": 50})), 201)
	plan := h.tool("create_plan", map[string]any{"input_ref": in.str("input_ref"), "destination_ref": "default", "selection_preset": "video"})
	if plan.IsError {
		t.Fatalf("plan: %+v", plan.Structured)
	}
	if h.fake.CallCount("torrents/add") != 0 {
		t.Fatal("planning must not add anything")
	}
	ref := plan.Structured["plan_ref"].(string)
	start := h.tool("start_download", map[string]any{"plan_ref": ref})
	if start.IsError || start.Structured["created"] != true {
		t.Fatalf("start: %+v", start.Structured)
	}
	id := start.Structured["task"].(map[string]any)["id"].(string)
	// Same plan again: the existing task, not a duplicate.
	if again := h.tool("start_download", map[string]any{"plan_ref": ref}); again.Structured["created"] != false {
		t.Fatalf("repeat: %+v", again.Structured)
	}
	// Same task store as the HTTP API.
	if v := h.task(id); v.str("added_via") != "app" {
		t.Fatal(string(v.raw))
	}
	if l := h.tool("list_downloads", nil); len(l.Structured["items"].([]any)) != 1 {
		t.Fatalf("list: %+v", l.Structured)
	}
	if p := h.tool("pause", map[string]any{"task_id": id}); p.IsError || p.Structured["state"] != "paused" {
		t.Fatalf("pause: %+v", p.Structured)
	}
	if g := h.tool("get_download", map[string]any{"task_id": id}); g.Structured["state"] != "paused" {
		t.Fatalf("get: %+v", g.Structured)
	}
	if r := h.tool("resume", map[string]any{"task_id": id}); r.IsError {
		t.Fatalf("resume: %+v", r.Structured)
	}
	rp := h.tool("create_remove_plan", map[string]any{"task_id": id})
	if rp.IsError {
		t.Fatalf("remove plan: %+v", rp.Structured)
	}
	if _, ok := h.fake.Get(id); !ok {
		t.Fatal("planning removal must not remove")
	}
	rm := h.tool("remove", map[string]any{"task_id": id, "plan_ref": rp.Structured["plan_ref"]})
	if rm.IsError || rm.Structured["removed"] != true || rm.Structured["data_deleted"] != false {
		t.Fatalf("remove: %+v", rm.Structured)
	}
	if g := h.tool("get_download", map[string]any{"task_id": id}); !g.IsError || g.code() != "task_not_found" {
		t.Fatalf("removed task: %+v", g.Structured)
	}
}

func TestMCPToolErrors(t *testing.T) {
	h := newHarness(t)
	if r := h.tool("create_plan", map[string]any{"magnet": "magnet:?xt=urn:btih:x", "destination_ref": "nope"}); !r.IsError || r.code() != "destination_not_allowed" {
		t.Fatalf("destination: %+v", r.Structured)
	}
	if r := h.tool("start_download", map[string]any{"plan_ref": "forged"}); r.code() != "plan_invalid" {
		t.Fatalf("forged plan: %+v", r.Structured)
	}
	if r := h.tool("pause", map[string]any{}); r.code() != "invalid_request" {
		t.Fatalf("missing id: %+v", r.Structured)
	}
	if r := h.tool("list_downloads", map[string]any{"extra": 1}); r.code() != "invalid_request" {
		t.Fatalf("unknown field: %+v", r.Structured)
	}
	// Plans are bound to the caller that created them.
	in := h.expect(h.do("POST", "/api/v1/torrent-inputs", makeTorrent("A", map[string]int{"a.mkv": 10})), 201)
	plan := h.tool("create_plan", map[string]any{"input_ref": in.str("input_ref"), "destination_ref": "default"})
	r := h.do("POST", "/api/mcp", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "start_download", "arguments": map[string]any{"plan_ref": plan.Structured["plan_ref"]}}},
		"Authorization", "Bearer other-token")
	if res := r.body["result"].(map[string]any); res["isError"] != true {
		t.Fatalf("other caller: %s", r.raw)
	}
}
