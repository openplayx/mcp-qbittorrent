package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apispec "github.com/openplayx/bittorrent-server/api"
	"github.com/openplayx/bittorrent-server/internal/api"
	"github.com/openplayx/bittorrent-server/internal/bencode"
	"github.com/openplayx/bittorrent-server/internal/config"
	"github.com/openplayx/bittorrent-server/internal/qbittorrent"
	"github.com/openplayx/bittorrent-server/internal/storage"
	"github.com/openplayx/bittorrent-server/internal/torrents"
	"github.com/openplayx/bittorrent-server/test/fakeqb"
)

const token = "test-token"

type harness struct {
	t     *testing.T
	fake  *fakeqb.Fake
	svc   *torrents.Service
	srv   *httptest.Server
	api   *api.Server
	cfg   *config.Config
	store *storage.Store
	free  int64
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	h := &harness{t: t, fake: fakeqb.New(), free: 100 << 30}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	cfg.DownloadRoot = root
	cfg.Engine.URL = h.fake.Server.URL
	cfg.Files.ContentAccess = true
	cfg.Limits.MaxTaskBytes = 1 << 20
	cfg.Limits.MinFreeBytes = 1000
	cfg.Limits.MaxDownloadRate = 5_000_000
	cfg.Destinations = []config.Destination{{Ref: "default", Name: "Test", SubPath: "test"}, {Ref: "small", Name: "Small", SubPath: "small", MaxTaskBytes: 10}}
	for _, m := range mutate {
		m(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h.cfg = &cfg
	store, err := storage.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	h.store = store
	engine, _ := qbittorrent.New(cfg.Engine.URL, "admin", "secret", 2*time.Second)
	if _, _, err := engine.Check(context.Background(), cfg.Engine.SupportedVersions, cfg.Engine.MinWebAPIVersion); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.svc = torrents.New(&cfg, store, engine, log, func(string) (int64, error) { return h.free, nil })
	h.svc.ConfirmTimeout, h.svc.ConfirmPoll = 2*time.Second, 20*time.Millisecond
	h.api = api.New(h.svc, &cfg, []config.APIToken{{Caller: "app", TokenSHA256: config.HashToken(token)}, {Caller: "other", TokenSHA256: config.HashToken("other-token")}}, log, "test")
	h.api.OperationWait = 2 * time.Second
	h.srv = httptest.NewServer(h.api)
	// Payload materialises on disk as the fake engine "downloads" it.
	h.fake.OnFileWritten = func(tr *fakeqb.Torrent, f fakeqb.File) {
		p := filepath.Join(tr.SavePath, filepath.FromSlash(f.Name))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, bytes.Repeat([]byte("z"), int(f.Size)), 0o644)
	}
	t.Cleanup(func() {
		h.srv.Close()
		h.svc.Close()
		store.Close()
		h.fake.Close()
	})
	h.svc.Reconcile(context.Background())
	return h
}

type resp struct {
	status int
	body   map[string]any
	raw    []byte
	header http.Header
}

func (h *harness) do(method, path string, body any, hdr ...string) resp {
	h.t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		rd, ct = bytes.NewReader(b), "application/x-bittorrent"
	default:
		raw, _ := json.Marshal(b)
		rd, ct = bytes.NewReader(raw), "application/json"
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{status: res.StatusCode, raw: raw, header: res.Header}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func (r resp) errCode() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func (r resp) str(k string) string { s, _ := r.body[k].(string); return s }

func (h *harness) expect(r resp, status int) resp {
	h.t.Helper()
	if r.status != status {
		h.t.Fatalf("status %d want %d: %s", r.status, status, r.raw)
	}
	return r
}

func makeTorrent(name string, files map[string]int) []byte {
	list := []any{}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		parts := []any{}
		for _, c := range strings.Split(k, "/") {
			parts = append(parts, c)
		}
		list = append(list, map[string]any{"length": files[k], "path": parts})
	}
	return bencode.Encode(map[string]any{"info": map[string]any{
		"name": name, "piece length": 16384, "pieces": strings.Repeat("p", 20), "files": list,
	}})
}

// createFromTorrent runs input → plan → create and returns task id.
func (h *harness) createFromTorrent(data []byte, preset string, autoStart bool) string {
	h.t.Helper()
	in := h.expect(h.do("POST", "/v1/torrent-inputs", data), 201)
	plan := h.expect(h.do("POST", "/v1/torrent-plans", map[string]any{
		"source": map[string]any{"type": "input", "input_ref": in.str("input_ref")}, "destination_ref": "default",
		"selection_preset": preset, "auto_start": autoStart,
	}), 201)
	op := h.expect(h.do("POST", "/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref"), "expected_revision": 1, "idempotency_key": "k-" + plan.str("plan_ref")}), 200)
	if op.str("status") != "succeeded" {
		h.t.Fatalf("create: %s", op.raw)
	}
	return op.str("task_id")
}

func (h *harness) task(id string) resp {
	h.t.Helper()
	return h.expect(h.do("GET", "/v1/torrents/"+id, nil), 200)
}

func (h *harness) tick(step float64) {
	h.fake.Tick(step)
	h.svc.Reconcile(context.Background())
}

func TestAuthHealthDocs(t *testing.T) {
	h := newHarness(t)
	h.expect(h.do("GET", "/healthz", nil, "Authorization", ""), 200)
	h.expect(h.do("GET", "/readyz", nil, "Authorization", ""), 200)
	h.expect(h.do("GET", "/docs", nil, "Authorization", ""), 200)
	if r := h.do("GET", "/v1/capabilities", nil, "Authorization", "Bearer wrong"); r.status != 401 || r.errCode() != "unauthorized" {
		t.Fatalf("bad token: %d %s", r.status, r.raw)
	}
	h.expect(h.do("GET", "/v1/torrents", nil, "Authorization", ""), 401)
	caps := h.expect(h.do("GET", "/v1/capabilities", nil), 200)
	if caps.body["engine"].(map[string]any)["version"] != "v5.1.0" || caps.body["file_content"] != true {
		t.Fatalf("caps %s", caps.raw)
	}
	for _, leak := range []string{h.cfg.DownloadRoot, "secret", h.fake.Server.URL} {
		if strings.Contains(string(caps.raw), leak) {
			t.Fatalf("capabilities leak %q", leak)
		}
	}
	if r := h.expect(h.do("GET", "/v1/destinations", nil), 200); len(r.body["destinations"].([]any)) != 2 {
		t.Fatal(string(r.raw))
	}
}

func TestOpenAPIMatchesRoutes(t *testing.T) {
	h := newHarness(t)
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(apispec.OpenAPI, &doc); err != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("spec: %v", err)
	}
	documented := map[string]bool{}
	for p, ops := range doc.Paths {
		for m := range ops {
			if m != "head" {
				documented[strings.ToUpper(m)+" "+p] = true
			}
		}
	}
	for _, r := range h.api.Routes {
		if !documented[r] {
			t.Errorf("route %s missing from openapi.json", r)
		}
		delete(documented, r)
	}
	for r := range documented {
		t.Errorf("openapi.json documents %s which is not served", r)
	}
}

func TestTorrentLifecycle(t *testing.T) {
	h := newHarness(t)
	data := makeTorrent("Show", map[string]int{"e1.mkv": 400, "e2.mkv": 300, "notes.txt": 50})
	id := h.createFromTorrent(data, "video", true)

	// Before reconciling metadata/selection the torrent is stopped.
	tr, _ := h.fake.Get(h.taskHash(id))
	if !tr.Stopped {
		t.Fatal("torrent file must be added stopped until selection is applied")
	}
	h.svc.Reconcile(context.Background()) // applies "video" preset and starts
	tr, _ = h.fake.Get(h.taskHash(id))
	if tr.Stopped || tr.Files[2].Priority != 0 || tr.Files[0].Priority != 1 {
		t.Fatalf("selection not applied: %+v", tr)
	}
	if !strings.HasPrefix(tr.SavePath, filepath.Join(h.cfg.DownloadRoot, "test", id)) {
		t.Fatalf("save path %s not isolated", tr.SavePath)
	}
	files := h.expect(h.do("GET", "/v1/torrents/"+id+"/files", nil), 200)
	if strings.Contains(string(files.raw), h.cfg.DownloadRoot) {
		t.Fatal("files leak absolute path")
	}
	h.tick(0.5)
	v := h.task(id)
	if v.str("state") != "downloading" || v.body["selected_files_complete"] == true {
		t.Fatalf("mid download %s", v.raw)
	}
	// Content of an incomplete file is refused.
	if r := h.do("GET", "/v1/torrents/"+id+"/files/f0/content", nil); r.status != 409 || r.errCode() != "file_incomplete" {
		t.Fatalf("incomplete content: %d %s", r.status, r.raw)
	}
	h.tick(0.6)
	v = h.task(id)
	if v.body["selected_files_complete"] != true || v.body["payload_complete"] == true || v.str("state") != "seeding" {
		t.Fatalf("partial selection must not claim payload complete: %s", v.raw)
	}

	// Range read of a complete file.
	r := h.do("GET", "/v1/torrents/"+id+"/files/f0/content", nil, "Range", "bytes=10-19")
	if r.status != 206 || len(r.raw) != 10 || r.header.Get("Content-Range") != "bytes 10-19/400" {
		t.Fatalf("range: %d %v", r.status, r.header)
	}
	head := h.do("HEAD", "/v1/torrents/"+id+"/files/f1/content", nil)
	if head.status != 200 || head.header.Get("Content-Length") != "300" {
		t.Fatalf("head: %d %v", head.status, head.header)
	}
	// Unselected file is not available.
	if r := h.do("GET", "/v1/torrents/"+id+"/files/f2/content", nil); r.status != 409 {
		t.Fatalf("unselected file served: %d", r.status)
	}
	// Other callers cannot see the task.
	if r := h.do("GET", "/v1/torrents/"+id, nil, "Authorization", "Bearer other-token"); r.status != 404 {
		t.Fatalf("cross-caller access: %d", r.status)
	}

	// Select the remaining file with the current revision; stale revision is refused.
	rev := int(h.task(id).body["files_revision"].(float64))
	if r := h.do("PATCH", "/v1/torrents/"+id+"/selection", map[string]any{"expected_revision": rev - 1, "preset": "all"}); r.status != 409 || r.errCode() != "files_revision_mismatch" {
		t.Fatalf("stale revision: %d %s", r.status, r.raw)
	}
	op := h.expect(h.do("PATCH", "/v1/torrents/"+id+"/selection", map[string]any{"expected_revision": rev, "file_ids": []string{"f0", "f1", "f2"}}), 200)
	if op.str("status") != "succeeded" {
		t.Fatal(string(op.raw))
	}
	h.tick(1)
	if v := h.task(id); v.body["payload_complete"] != true {
		t.Fatalf("payload complete: %s", v.raw)
	}

	// Pause waits for the engine to actually stop.
	op = h.expect(h.do("POST", "/v1/torrents/"+id+"/pause", nil), 200)
	if op.str("status") != "succeeded" || op.body["result"].(map[string]any)["state"] != "completed" {
		t.Fatalf("pause %s", op.raw)
	}
	op = h.expect(h.do("POST", "/v1/torrents/"+id+"/resume", nil), 200)
	if op.str("status") != "succeeded" {
		t.Fatalf("resume %s", op.raw)
	}
	// Limits within the ceiling; above it is refused.
	op = h.expect(h.do("PATCH", "/v1/torrents/"+id+"/limits", map[string]any{"download_limit": 1000, "upload_limit": 0}), 200)
	if op.str("status") != "succeeded" {
		t.Fatalf("limits %s", op.raw)
	}
	if tr, _ := h.fake.Get(h.taskHash(id)); tr.DlLimit != 1000 {
		t.Fatalf("engine dl limit %d", tr.DlLimit)
	}
	if r := h.do("PATCH", "/v1/torrents/"+id+"/limits", map[string]any{"download_limit": 9_000_000}); r.status != 422 {
		t.Fatalf("limit above ceiling: %d", r.status)
	}

	// Remove keeping files.
	rp := h.expect(h.do("POST", "/v1/torrents/"+id+"/remove-plan", map[string]any{"delete_files": false}), 201)
	if rp.body["bytes_on_disk"].(float64) != 750 {
		t.Fatalf("remove plan %s", rp.raw)
	}
	op = h.expect(h.do("POST", "/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": rp.str("plan_ref"), "idempotency_key": "rm1"}), 200)
	if op.str("status") != "succeeded" {
		t.Fatalf("remove %s", op.raw)
	}
	if _, ok := h.fake.Get(h.taskHashFromStore(id)); ok {
		t.Fatal("engine still has torrent")
	}
	if _, err := os.Stat(filepath.Join(h.cfg.DownloadRoot, "test", id, "Show", "e1.mkv")); err != nil {
		t.Fatal("files were not retained")
	}
	if v := h.task(id); v.str("state") != "removed" {
		t.Fatal(string(v.raw))
	}
	// Plan reuse is refused; the same idempotency key replays the result.
	if r := h.do("POST", "/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": rp.str("plan_ref"), "idempotency_key": "rm1"}); r.str("id") != op.str("id") {
		t.Fatalf("idempotent replay: %s", r.raw)
	}
	if r := h.do("POST", "/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": rp.str("plan_ref"), "idempotency_key": "rm2"}); r.status != 409 {
		t.Fatalf("removed task: %d %s", r.status, r.raw)
	}
}

// taskHash reads the engine hash via the fake (by identity of the only torrent with this task's tag).
func (h *harness) taskHash(id string) string {
	h.t.Helper()
	return h.taskHashFromStore(id)
}

func (h *harness) taskHashFromStore(id string) string {
	h.t.Helper()
	row, err := h.store.GetTaskAny(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	if row.V1 != "" {
		return row.V1
	}
	return row.V2
}

func TestDeleteFilesOnlyTouchesTaskDir(t *testing.T) {
	h := newHarness(t)
	a := h.createFromTorrent(makeTorrent("A", map[string]int{"a.mkv": 10}), "all", true)
	b := h.createFromTorrent(makeTorrent("B", map[string]int{"b.mkv": 10}), "all", true)
	h.tick(0) // apply selection and start
	h.tick(1)
	rp := h.expect(h.do("POST", "/v1/torrents/"+a+"/remove-plan", map[string]any{"delete_files": true}), 201)
	if rp.body["delete_files"] != true || len(rp.body["shared_with_tasks"].([]any)) != 0 {
		t.Fatal(string(rp.raw))
	}
	// A mutation after the preview invalidates the plan.
	h.expect(h.do("POST", "/v1/torrents/"+a+"/pause", nil), 200)
	if r := h.do("POST", "/v1/torrents/"+a+"/remove", map[string]any{"plan_ref": rp.str("plan_ref"), "idempotency_key": "x"}); r.errCode() != "task_changed_since_plan" {
		t.Fatalf("stale remove plan: %s", r.raw)
	}
	rp = h.expect(h.do("POST", "/v1/torrents/"+a+"/remove-plan", map[string]any{"delete_files": true}), 201)
	op := h.expect(h.do("POST", "/v1/torrents/"+a+"/remove", map[string]any{"plan_ref": rp.str("plan_ref"), "idempotency_key": "y"}), 200)
	if op.str("status") != "succeeded" || op.body["result"].(map[string]any)["data_deleted"] != true {
		t.Fatal(string(op.raw))
	}
	if _, err := os.Stat(filepath.Join(h.cfg.DownloadRoot, "test", a)); !os.IsNotExist(err) {
		t.Fatal("task A dir still exists")
	}
	if _, err := os.Stat(filepath.Join(h.cfg.DownloadRoot, "test", b, "B", "b.mkv")); err != nil {
		t.Fatal("task B data touched")
	}
}

func TestIdempotencyAndDuplicates(t *testing.T) {
	h := newHarness(t)
	data := makeTorrent("Dup", map[string]int{"x.mkv": 10})
	in := h.expect(h.do("POST", "/v1/torrent-inputs", data), 201)
	planBody := map[string]any{"source": map[string]any{"type": "input", "input_ref": in.str("input_ref")}, "destination_ref": "default"}
	plan := h.expect(h.do("POST", "/v1/torrent-plans", planBody), 201)
	req := map[string]any{"plan_ref": plan.str("plan_ref"), "expected_revision": 1}
	if r := h.do("POST", "/v1/torrents", req); r.errCode() != "idempotency_key_required" {
		t.Fatal(string(r.raw))
	}
	first := h.expect(h.do("POST", "/v1/torrents", req, "Idempotency-Key", "same"), 200)
	again := h.expect(h.do("POST", "/v1/torrents", req, "Idempotency-Key", "same"), 200)
	if first.str("id") != again.str("id") || first.str("task_id") != again.str("task_id") {
		t.Fatalf("replay differs: %s vs %s", first.raw, again.raw)
	}
	if h.fake.CallCount("torrents/add") != 1 {
		t.Fatalf("add called %d times", h.fake.CallCount("torrents/add"))
	}
	req["expected_revision"] = 2
	if r := h.do("POST", "/v1/torrents", req, "Idempotency-Key", "same"); r.status != 409 || r.errCode() != "idempotency_key_reused" {
		t.Fatalf("same key different params: %d %s", r.status, r.raw)
	}
	// Same payload again is refused, pointing to the existing task.
	r := h.do("POST", "/v1/torrent-plans", planBody)
	if r.status != 409 || r.errCode() != "duplicate_task" {
		t.Fatalf("duplicate: %d %s", r.status, r.raw)
	}
}

func TestEngineConflictAndLostAddReply(t *testing.T) {
	h := newHarness(t)
	ext := makeTorrent("Ext", map[string]int{"e.mkv": 10})
	in := h.expect(h.do("POST", "/v1/torrent-inputs", ext), 201)
	plan := h.expect(h.do("POST", "/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "input", "input_ref": in.str("input_ref")}, "destination_ref": "default"}), 201)
	// Someone adds the same content directly in the engine: never adopted.
	root, _ := bencode.Decode(ext)
	_ = root
	inp, _ := torrents.ParseTorrent(context.Background(), ext, torrents.Policy{TrackerSchemes: []string{"http"}})
	h.fake.AddExternal(fakeqb.Torrent{V1: inp.Identity.V1, Name: "Ext", HasMetadata: true})
	r := h.do("POST", "/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref"), "expected_revision": 1, "idempotency_key": "c1"})
	if r.status != 409 || r.errCode() != "engine_conflict" {
		t.Fatalf("engine conflict: %d %s", r.status, r.raw)
	}
	if list := h.expect(h.do("GET", "/v1/torrents", nil), 200); len(list.body["items"].([]any)) != 0 {
		t.Fatal("external torrent exposed")
	}

	// The add reply is lost (500) but the engine did add it: reconcile, don't retry.
	h.fake.Set(func(f *fakeqb.Fake) { f.DropAddReply = true })
	id := h.createFromTorrent(makeTorrent("Lost", map[string]int{"l.mkv": 10}), "all", true)
	if h.fake.CallCount("torrents/add") != 1 {
		t.Fatalf("add retried: %d", h.fake.CallCount("torrents/add"))
	}
	if v := h.task(id); v.str("state") == "error" {
		t.Fatal(string(v.raw))
	}

	// Definite rejection fails the operation without a task in the engine.
	h.fake.Set(func(f *fakeqb.Fake) { f.DropAddReply, f.RejectAdd = false, true })
	in = h.expect(h.do("POST", "/v1/torrent-inputs", makeTorrent("Rej", map[string]int{"r.mkv": 10})), 201)
	plan = h.expect(h.do("POST", "/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "input", "input_ref": in.str("input_ref")}, "destination_ref": "default"}), 201)
	op := h.expect(h.do("POST", "/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref"), "expected_revision": 1, "idempotency_key": "rej"}), 200)
	if op.str("status") != "failed" || op.str("error_code") != "engine_rejected" {
		t.Fatalf("reject: %s", op.raw)
	}
}

func TestMagnetMetadataFirst(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.AllowPrivateNetworks = true })
	hash := strings.Repeat("ab", 20)
	h.fake.RegisterMetadata(hash, []fakeqb.File{{Name: "Movie/movie.mkv", Size: 500}, {Name: "Movie/sample.mkv", Size: 20}, {Name: "Movie/info.nfo", Size: 1}})
	plan := h.expect(h.do("POST", "/v1/torrent-plans", map[string]any{
		"source":          map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + hash + "&dn=Movie&tr=http://192.168.1.5:9000/announce"},
		"destination_ref": "default", "selection_preset": "video",
	}), 201)
	if plan.str("metadata") != "unknown" || !strings.Contains(string(plan.raw), "metadata_unknown") || strings.Contains(string(plan.raw), "magnet:") {
		t.Fatalf("plan %s", plan.raw)
	}
	op := h.expect(h.do("POST", "/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref"), "expected_revision": 1, "idempotency_key": "m"}), 200)
	id := op.str("task_id")
	if v := h.task(id); v.str("state") != "metadata_pending" {
		t.Fatal(string(v.raw))
	}
	tr, _ := h.fake.Get(hash)
	if !tr.StopOnMetadata || tr.Stopped {
		t.Fatalf("magnet must use stopCondition=MetadataReceived: %+v", tr)
	}
	if r := h.do("PATCH", "/v1/torrents/"+id+"/selection", map[string]any{"expected_revision": 0, "preset": "all"}); r.errCode() != "metadata_pending" {
		t.Fatalf("select before metadata: %s", r.raw)
	}
	h.fake.Tick(0) // metadata arrives; engine stops itself
	h.svc.Reconcile(context.Background())
	tr, _ = h.fake.Get(hash)
	if tr.Stopped || tr.Files[0].Priority != 1 || tr.Files[1].Priority != 0 || tr.Files[2].Priority != 0 {
		t.Fatalf("video preset after metadata: %+v", tr)
	}
	h.tick(1)
	h.tick(0)
	v := h.task(id)
	if v.body["selected_files_complete"] != true || v.body["payload_complete"] == true {
		t.Fatal(string(v.raw))
	}
}

func TestQuotaAndAwaitingSelection(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.AllowPrivateNetworks = true })
	// Known size above the destination quota is refused at plan time.
	in := h.expect(h.do("POST", "/v1/torrent-inputs", makeTorrent("Big", map[string]int{"big.mkv": 100})), 201)
	r := h.do("POST", "/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "input", "input_ref": in.str("input_ref")}, "destination_ref": "small"})
	if r.errCode() != "quota_exceeded" {
		t.Fatal(string(r.raw))
	}
	// Magnet whose metadata turns out too large is held, not started.
	hash := strings.Repeat("cd", 20)
	h.fake.RegisterMetadata(hash, []fakeqb.File{{Name: "Huge/a.mkv", Size: 5 << 20}})
	plan := h.expect(h.do("POST", "/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + hash}, "destination_ref": "default"}), 201)
	id := h.expect(h.do("POST", "/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref"), "expected_revision": 1, "idempotency_key": "q"}), 200).str("task_id")
	h.fake.Tick(0)
	h.svc.Reconcile(context.Background())
	v := h.task(id)
	if v.str("hold_reason") != "quota_exceeded" || v.str("state") != "paused" {
		t.Fatalf("quota hold: %s", v.raw)
	}
	if r := h.do("POST", "/v1/torrents/"+id+"/resume", nil); r.errCode() != "quota_exceeded" {
		t.Fatalf("resume over quota: %s", r.raw)
	}
	// auto_start=false waits for an explicit resume.
	id2 := h.createFromTorrent(makeTorrent("Wait", map[string]int{"w.mkv": 10}), "all", false)
	h.svc.Reconcile(context.Background())
	if v := h.task(id2); v.str("hold_reason") != "awaiting_selection" || v.str("state") != "paused" {
		t.Fatal(string(v.raw))
	}
	h.expect(h.do("POST", "/v1/torrents/"+id2+"/resume", nil), 200)
	if tr, _ := h.fake.Get(h.taskHash(id2)); tr.Stopped {
		t.Fatal("not started after resume")
	}
}

func TestEngineOutageAndOutsideRemoval(t *testing.T) {
	h := newHarness(t)
	id := h.createFromTorrent(makeTorrent("O", map[string]int{"o.mkv": 10}), "all", true)
	h.tick(0.3)
	h.fake.Set(func(f *fakeqb.Fake) { f.Down = true })
	h.svc.Reconcile(context.Background())
	v := h.task(id)
	if v.body["stale"] != true || v.str("engine_status") != "unavailable" || v.body["download_speed"].(float64) != 0 {
		t.Fatalf("outage: %s", v.raw)
	}
	if r := h.do("GET", "/readyz", nil); r.status != 503 {
		t.Fatal("readyz should fail")
	}
	h.fake.Set(func(f *fakeqb.Fake) { f.Down = false })
	h.fake.ExpireSessions() // engine restarted: SID invalid, adapter re-logs in
	h.svc.Reconcile(context.Background())
	if v := h.task(id); v.body["stale"] == true || v.str("state") != "downloading" {
		t.Fatalf("recovered: %s", v.raw)
	}
	h.fake.RemoveOutside(h.taskHash(id))
	h.svc.Reconcile(context.Background())
	v = h.task(id)
	if v.str("state") != "error" || v.str("error_code") != "engine_task_missing" {
		t.Fatalf("missing: %s", v.raw)
	}
	if h.fake.CallCount("torrents/add") != 1 {
		t.Fatal("missing task was re-added")
	}
}

func TestPauseUnconfirmed(t *testing.T) {
	h := newHarness(t)
	id := h.createFromTorrent(makeTorrent("P", map[string]int{"p.mkv": 10}), "all", true)
	h.svc.Reconcile(context.Background())
	h.fake.Set(func(f *fakeqb.Fake) { f.IgnoreStop = true })
	op := h.do("POST", "/v1/torrents/"+id+"/pause", nil)
	if op.str("status") == "succeeded" {
		t.Fatal("pause reported success though engine did not stop")
	}
	deadline := time.Now().Add(5 * time.Second)
	for op.str("status") == "pending" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		op = h.do("GET", "/v1/operations/"+op.str("id"), nil)
	}
	if op.str("status") != "failed" || op.str("error_code") != "engine_state_unconfirmed" {
		t.Fatalf("unconfirmed pause: %s", op.raw)
	}
}

func TestRecoverInterruptedCreate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Simulate a crash between "operation started" and confirmation.
	_, _, err := h.store.BeginOperation(ctx, storage.Operation{ID: "op_x", Caller: "app", Kind: "pause", IdemKey: "k", RequestDigest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	op := h.expect(h.do("GET", "/v1/operations/op_x", nil), 200)
	if op.str("status") != "failed" || op.str("error_code") != "operation_interrupted" {
		t.Fatal(string(op.raw))
	}
}

func TestInputValidation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.MaxTorrentFileBytes = 300 })
	if r := h.do("POST", "/v1/torrent-inputs", bytes.Repeat([]byte("a"), 400)); r.status != 413 {
		t.Fatalf("size limit: %d", r.status)
	}
	if r := h.do("POST", "/v1/torrent-inputs", []byte("garbage")); r.errCode() != "invalid_torrent" {
		t.Fatal(string(r.raw))
	}
	cases := []map[string]any{
		{"source": map[string]any{"type": "url", "magnet": "http://x"}, "destination_ref": "default"},
		{"source": map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40), "input_ref": "x"}, "destination_ref": "default"},
		{"source": map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, "destination_ref": "../etc"},
		{"source": map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, "destination_ref": "default", "save_path": "/tmp"},
	}
	for i, c := range cases {
		if r := h.do("POST", "/v1/torrent-plans", c); r.status < 400 {
			t.Errorf("case %d accepted: %s", i, r.raw)
		}
	}
	if r := h.do("POST", "/v1/torrent-plans", "x", "Content-Type", "text/plain"); r.status != 415 {
		t.Errorf("content type: %d", r.status)
	}
}

func TestListPagination(t *testing.T) {
	h := newHarness(t)
	ids := []string{}
	for i := range 5 {
		ids = append(ids, h.createFromTorrent(makeTorrent(fmt.Sprintf("T%d", i), map[string]int{"x.mkv": 10 + i}), "all", true))
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/v1/torrents?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := h.expect(h.do("GET", path, nil), 200)
		for _, it := range r.body["items"].([]any) {
			seen[it.(map[string]any)["id"].(string)] = true
		}
		cursor = r.str("next_cursor")
		if cursor == "" {
			break
		}
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("pagination missed %s", id)
		}
	}
}

func TestRateLimit(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.RequestsPerMinute = 3 })
	for range 3 {
		h.expect(h.do("GET", "/v1/capabilities", nil), 200)
	}
	if r := h.do("GET", "/v1/capabilities", nil); r.status != 429 || r.header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", r.status)
	}
}
