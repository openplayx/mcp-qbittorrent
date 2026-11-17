package api_test

import (
	"bytes"
	"context"
	"encoding/json"
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

	apispec "github.com/openplayx/bittorrent-server/src/api"
	"github.com/openplayx/bittorrent-server/src/internal/api"
	"github.com/openplayx/bittorrent-server/src/internal/bencode"
	"github.com/openplayx/bittorrent-server/src/internal/config"
	"github.com/openplayx/bittorrent-server/src/internal/qbittorrent"
	"github.com/openplayx/bittorrent-server/src/internal/torrents"
	"github.com/openplayx/bittorrent-server/src/test/fakeqb"
)

const token = "test-token"

type harness struct {
	t    *testing.T
	fake *fakeqb.Fake
	svc  *torrents.Service
	srv  *httptest.Server
	api  *api.Server
	cfg  *config.Config
	free int64
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	h := &harness{t: t, fake: fakeqb.New(), free: 100 << 30}
	cfg := config.Default()
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
	engine, _ := qbittorrent.New(cfg.Engine.URL, "admin", "secret", 2*time.Second)
	if _, _, err := engine.Check(context.Background(), cfg.Engine.SupportedVersions, cfg.Engine.MinWebAPIVersion); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.svc = torrents.New(&cfg, engine, log, func(string) (int64, error) { return h.free, nil })
	h.svc.ConfirmTimeout, h.svc.ConfirmPoll = 2*time.Second, 20*time.Millisecond
	h.api = api.New(h.svc, &cfg, []config.APIToken{{Caller: "app", TokenSHA256: config.HashToken(token)}, {Caller: "other", TokenSHA256: config.HashToken("other-token")}}, log, "test")
	h.srv = httptest.NewServer(h.api)
	// Payload materialises on disk as the fake engine "downloads" it.
	h.fake.OnFileWritten = func(tr *fakeqb.Torrent, f fakeqb.File) {
		p := filepath.Join(tr.SavePath, filepath.FromSlash(f.Name))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, bytes.Repeat([]byte("z"), int(f.Size)), 0o644)
	}
	h.fake.OnDeleteFiles = func(tr *fakeqb.Torrent) {
		for _, f := range tr.Files {
			_ = os.Remove(filepath.Join(tr.SavePath, filepath.FromSlash(f.Name)))
		}
		if len(tr.Files) > 0 {
			_ = os.Remove(filepath.Join(tr.SavePath, strings.Split(tr.Files[0].Name, "/")[0]))
		}
	}
	t.Cleanup(func() {
		h.srv.Close()
		h.fake.Close()
	})
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

func (h *harness) plan(data []byte, preset string, autoStart bool) resp {
	h.t.Helper()
	in := h.expect(h.do("POST", "/api/v1/torrent-inputs", data), 201)
	return h.expect(h.do("POST", "/api/v1/torrent-plans", map[string]any{
		"source": map[string]any{"type": "input", "input_ref": in.str("input_ref")}, "destination_ref": "default",
		"selection_preset": preset, "auto_start": autoStart,
	}), 201)
}

// create runs input → plan → create and returns the engine hash id.
func (h *harness) create(data []byte, preset string, autoStart bool) string {
	h.t.Helper()
	plan := h.plan(data, preset, autoStart)
	return h.expect(h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref")}), 201).str("id")
}

func (h *harness) task(id string) resp {
	h.t.Helper()
	return h.expect(h.do("GET", "/api/v1/torrents/"+id, nil), 200)
}

func (h *harness) tick(step float64) {
	h.fake.Tick(step)
	h.svc.Reconcile(context.Background())
}

func (h *harness) files(id string) resp {
	h.t.Helper()
	return h.expect(h.do("GET", "/api/v1/torrents/"+id+"/files", nil), 200)
}

func TestAuthHealthDocs(t *testing.T) {
	h := newHarness(t)
	h.expect(h.do("GET", "/api/healthz", nil, "Authorization", ""), 200)
	h.expect(h.do("GET", "/api/readyz", nil, "Authorization", ""), 200)
	h.expect(h.do("GET", "/api/docs", nil, "Authorization", ""), 200)
	if r := h.do("GET", "/api/v1/capabilities", nil, "Authorization", "Bearer wrong"); r.status != 401 || r.errCode() != "unauthorized" {
		t.Fatalf("bad token: %d %s", r.status, r.raw)
	}
	h.expect(h.do("GET", "/api/v1/torrents", nil, "Authorization", ""), 401)
	caps := h.expect(h.do("GET", "/api/v1/capabilities", nil), 200)
	if caps.body["engine"].(map[string]any)["version"] != "v5.1.0" || caps.body["state_storage"] != "none" {
		t.Fatalf("caps %s", caps.raw)
	}
	for _, leak := range []string{h.cfg.DownloadRoot, "secret", h.fake.Server.URL} {
		if strings.Contains(string(caps.raw), leak) {
			t.Fatalf("capabilities leak %q", leak)
		}
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
	id := h.create(makeTorrent("Show", map[string]int{"e1.mkv": 400, "e2.mkv": 300, "notes.txt": 50}), "video", true)
	// The initial selection is applied before payload transfer starts.
	tr, _ := h.fake.Get(id)
	if tr.Stopped || tr.Files[2].Priority != 0 || tr.Files[0].Priority != 1 || slices.Contains(tr.Tags, torrents.TagPending) {
		t.Fatalf("selection not applied: %+v", tr)
	}
	if !strings.HasPrefix(tr.SavePath, filepath.Join(h.cfg.DownloadRoot, "test", "opx-")) {
		t.Fatalf("save path %s not isolated", tr.SavePath)
	}
	if v := h.task(id); v.str("added_via") != "app" || v.str("destination_ref") != "default" {
		t.Fatal(string(v.raw))
	}
	if strings.Contains(string(h.files(id).raw), h.cfg.DownloadRoot) {
		t.Fatal("files leak absolute path")
	}
	h.tick(0.5)
	v := h.task(id)
	if v.str("state") != "downloading" || v.body["selected_files_complete"] == true {
		t.Fatalf("mid download %s", v.raw)
	}
	if r := h.do("GET", "/api/v1/torrents/"+id+"/files/f0/content", nil); r.status != 409 || r.errCode() != "file_incomplete" {
		t.Fatalf("incomplete content: %d %s", r.status, r.raw)
	}
	h.tick(0.6)
	v = h.task(id)
	if v.body["selected_files_complete"] != true || v.body["payload_complete"] == true || v.str("state") != "seeding" {
		t.Fatalf("partial selection must not claim payload complete: %s", v.raw)
	}
	r := h.do("GET", "/api/v1/torrents/"+id+"/files/f0/content", nil, "Range", "bytes=10-19")
	if r.status != 206 || len(r.raw) != 10 || r.header.Get("Content-Range") != "bytes 10-19/400" {
		t.Fatalf("range: %d %v", r.status, r.header)
	}
	if head := h.do("HEAD", "/api/v1/torrents/"+id+"/files/f1/content", nil); head.status != 200 || head.header.Get("Content-Length") != "300" {
		t.Fatalf("head: %d %v", head.status, head.header)
	}

	rev := h.files(id).str("files_revision")
	if r := h.do("PATCH", "/api/v1/torrents/"+id+"/selection", map[string]any{"expected_revision": "stale", "preset": "all"}); r.status != 409 || r.errCode() != "files_revision_mismatch" {
		t.Fatalf("stale revision: %d %s", r.status, r.raw)
	}
	sel := h.expect(h.do("PATCH", "/api/v1/torrents/"+id+"/selection", map[string]any{"expected_revision": rev, "file_ids": []string{"f0", "f1", "f2"}}), 200)
	if sel.str("files_revision") == rev {
		t.Fatal("revision unchanged after selection")
	}
	h.tick(1)
	if v := h.task(id); v.body["payload_complete"] != true {
		t.Fatalf("payload complete: %s", v.raw)
	}

	p := h.expect(h.do("POST", "/api/v1/torrents/"+id+"/pause", nil), 200)
	if p.str("state") != "completed" {
		t.Fatalf("pause %s", p.raw)
	}
	if r := h.expect(h.do("POST", "/api/v1/torrents/"+id+"/resume", nil), 200); r.str("state") != "seeding" {
		t.Fatalf("resume %s", r.raw)
	}
	if l := h.expect(h.do("PATCH", "/api/v1/torrents/"+id+"/limits", map[string]any{"download_limit": 1000, "upload_limit": 0}), 200); l.body["download_limit"].(float64) != 1000 {
		t.Fatal(string(l.raw))
	}
	if r := h.do("PATCH", "/api/v1/torrents/"+id+"/limits", map[string]any{"download_limit": 9_000_000}); r.status != 422 {
		t.Fatalf("limit above ceiling: %d", r.status)
	}

	rp := h.expect(h.do("POST", "/api/v1/torrents/"+id+"/remove-plan", map[string]any{"delete_files": false}), 201)
	if rp.body["bytes_on_disk"].(float64) != 750 {
		t.Fatalf("remove plan %s", rp.raw)
	}
	// A remove plan only applies to its own torrent.
	if r := h.do("POST", "/api/v1/torrents/"+strings.Repeat("0", 40)+"/remove", map[string]any{"plan_ref": rp.str("plan_ref")}); r.errCode() != "remove_plan_invalid" {
		t.Fatalf("plan reuse on other id: %s", r.raw)
	}
	rm := h.expect(h.do("POST", "/api/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": rp.str("plan_ref")}), 200)
	if rm.body["removed"] != true || rm.body["data_deleted"] != false {
		t.Fatal(string(rm.raw))
	}
	if _, ok := h.fake.Get(id); ok {
		t.Fatal("engine still has torrent")
	}
	if _, err := os.Stat(filepath.Join(tr.SavePath, "Show", "e1.mkv")); err != nil {
		t.Fatal("files were not retained")
	}
	h.expect(h.do("GET", "/api/v1/torrents/"+id, nil), 404)
}

func TestAllEngineTorrentsAreListedAndControllable(t *testing.T) {
	h := newHarness(t)
	mine := h.create(makeTorrent("Mine", map[string]int{"m.mkv": 10}), "all", true)
	webui := strings.Repeat("ab", 20)
	h.fake.AddExternal(fakeqb.Torrent{V1: webui, Name: "Added in WebUI", HasMetadata: true, SavePath: h.cfg.DownloadRoot + "/.unassigned",
		Files: []fakeqb.File{{Name: "w.mkv", Size: 20, Priority: 1}}})
	list := h.expect(h.do("GET", "/api/v1/torrents", nil), 200)
	items := list.body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("list: %s", list.raw)
	}
	via := map[string]string{}
	for _, it := range items {
		m := it.(map[string]any)
		via[m["id"].(string)] = m["added_via"].(string)
	}
	if via[mine] != "app" || via[webui] != "other" {
		t.Fatalf("added_via: %v", via)
	}
	if strings.Contains(string(list.raw), h.cfg.DownloadRoot) {
		t.Fatal("list leaks paths")
	}
	if p := h.expect(h.do("POST", "/api/v1/torrents/"+webui+"/pause", nil), 200); p.str("state") != "paused" {
		t.Fatal(string(p.raw))
	}
	h.expect(h.do("POST", "/api/v1/torrents/"+webui+"/resume", nil), 200)
	rp := h.expect(h.do("POST", "/api/v1/torrents/"+webui+"/remove-plan", nil), 201)
	h.expect(h.do("POST", "/api/v1/torrents/"+webui+"/remove", map[string]any{"plan_ref": rp.str("plan_ref")}), 200)
	if _, ok := h.fake.Get(webui); ok {
		t.Fatal("webui torrent not removed")
	}
	// Reads are live: a change made directly in the engine shows at once.
	h.fake.RemoveOutside(mine)
	h.expect(h.do("GET", "/api/v1/torrents/"+mine, nil), 404)
	if r := h.do("GET", "/api/v1/torrents/not-a-hash", nil); r.status != 404 {
		t.Fatalf("invalid id: %d", r.status)
	}
}

func TestDeleteFilesOnlyThatTorrent(t *testing.T) {
	h := newHarness(t)
	a := h.create(makeTorrent("A", map[string]int{"a.mkv": 10}), "all", true)
	b := h.create(makeTorrent("B", map[string]int{"b.mkv": 10}), "all", true)
	h.tick(1)
	ta, _ := h.fake.Get(a)
	tb, _ := h.fake.Get(b)
	rp := h.expect(h.do("POST", "/api/v1/torrents/"+a+"/remove-plan", map[string]any{"delete_files": true}), 201)
	rm := h.expect(h.do("POST", "/api/v1/torrents/"+a+"/remove", map[string]any{"plan_ref": rp.str("plan_ref")}), 200)
	if rm.body["data_deleted"] != true {
		t.Fatal(string(rm.raw))
	}
	// The empty directory is tidied up in the background.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ta.SavePath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task A dir still exists")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(tb.SavePath, "B", "b.mkv")); err != nil {
		t.Fatal("task B data touched")
	}
}

func TestRemovePlanBoundToTorrentInstance(t *testing.T) {
	h := newHarness(t)
	id := h.create(makeTorrent("Again", map[string]int{"x.mkv": 10}), "all", true)
	rp := h.expect(h.do("POST", "/api/v1/torrents/"+id+"/remove-plan", nil), 201)
	// Removed and re-added outside: the old confirmation must not apply.
	tr, _ := h.fake.Get(id)
	h.fake.RemoveOutside(id)
	tr.Hash = ""
	h.fake.AddExternal(tr)
	if r := h.do("POST", "/api/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": rp.str("plan_ref")}); r.errCode() != "task_changed_since_plan" {
		t.Fatalf("replayed plan: %s", r.raw)
	}
	if r := h.do("POST", "/api/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": "rm.forged"}); r.errCode() != "remove_plan_invalid" {
		t.Fatalf("forged plan: %s", r.raw)
	}
}

func TestCreateIsIdempotentAndDeduplicated(t *testing.T) {
	h := newHarness(t)
	data := makeTorrent("Dup", map[string]int{"x.mkv": 10})
	plan := h.plan(data, "all", true)
	first := h.expect(h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref")}), 201)
	again := h.expect(h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref")}), 200)
	if first.str("id") != again.str("id") || h.fake.CallCount("torrents/add") != 1 {
		t.Fatalf("not idempotent: %s / %s", first.raw, again.raw)
	}
	in := h.expect(h.do("POST", "/api/v1/torrent-inputs", data), 201)
	r := h.do("POST", "/api/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "input", "input_ref": in.str("input_ref")}, "destination_ref": "default"})
	if r.status != 409 || r.errCode() != "duplicate_task" || r.body["error"].(map[string]any)["detail"].(map[string]any)["task_id"] != first.str("id") {
		t.Fatalf("duplicate: %d %s", r.status, r.raw)
	}
	// Plans are bound to the caller and cannot be forged.
	other := h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref")}, "Authorization", "Bearer other-token")
	if other.errCode() != "plan_invalid" {
		t.Fatalf("cross-caller plan: %s", other.raw)
	}
	if r := h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": "plan.AAAA"}); r.errCode() != "plan_invalid" {
		t.Fatalf("forged plan: %s", r.raw)
	}
}

func TestLostAddReplyAndRejection(t *testing.T) {
	h := newHarness(t)
	h.fake.Set(func(f *fakeqb.Fake) { f.DropAddReply = true })
	id := h.create(makeTorrent("Lost", map[string]int{"l.mkv": 10}), "all", true)
	if h.fake.CallCount("torrents/add") != 1 || h.task(id).str("state") == "error" {
		t.Fatal("lost reply not reconciled")
	}
	h.fake.Set(func(f *fakeqb.Fake) { f.DropAddReply, f.RejectAdd = false, true })
	plan := h.plan(makeTorrent("Rej", map[string]int{"r.mkv": 10}), "all", true)
	if r := h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref")}); r.status != 409 || r.errCode() != "engine_rejected" {
		t.Fatalf("reject: %d %s", r.status, r.raw)
	}
}

func TestMagnetMetadataFirst(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.AllowPrivateNetworks = true })
	hash := strings.Repeat("ab", 20)
	h.fake.RegisterMetadata(hash, []fakeqb.File{{Name: "Movie/movie.mkv", Size: 500}, {Name: "Movie/sample.mkv", Size: 20}, {Name: "Movie/info.nfo", Size: 1}})
	plan := h.expect(h.do("POST", "/api/v1/torrent-plans", map[string]any{
		"source":          map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + hash + "&dn=Movie&tr=http://192.168.1.5:9000/announce"},
		"destination_ref": "default", "selection_preset": "video",
	}), 201)
	if plan.str("metadata") != "unknown" || strings.Contains(string(plan.raw), "magnet:") {
		t.Fatalf("plan %s", plan.raw)
	}
	id := h.expect(h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref")}), 201).str("id")
	if v := h.task(id); v.str("state") != "metadata_pending" {
		t.Fatal(string(v.raw))
	}
	tr, _ := h.fake.Get(id)
	if !tr.StopOnMetadata || tr.Stopped || !slices.Contains(tr.Tags, torrents.TagPending) {
		t.Fatalf("magnet must use stopCondition=MetadataReceived and pending tag: %+v", tr)
	}
	h.fake.Tick(0) // metadata arrives; engine stops itself
	h.svc.Reconcile(context.Background())
	tr, _ = h.fake.Get(id)
	if tr.Stopped || tr.Files[0].Priority != 1 || tr.Files[1].Priority != 0 || tr.Files[2].Priority != 0 || slices.Contains(tr.Tags, torrents.TagPending) {
		t.Fatalf("video preset after metadata: %+v", tr)
	}
	h.tick(1)
	if v := h.task(id); v.body["selected_files_complete"] != true || v.body["payload_complete"] == true {
		t.Fatal(string(v.raw))
	}
}

func TestHoldsLiveInEngineTags(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Network.AllowPrivateNetworks = true })
	in := h.expect(h.do("POST", "/api/v1/torrent-inputs", makeTorrent("Big", map[string]int{"big.mkv": 100})), 201)
	if r := h.do("POST", "/api/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "input", "input_ref": in.str("input_ref")}, "destination_ref": "small"}); r.errCode() != "quota_exceeded" {
		t.Fatal(string(r.raw))
	}
	hash := strings.Repeat("cd", 20)
	h.fake.RegisterMetadata(hash, []fakeqb.File{{Name: "Huge/a.mkv", Size: 5 << 20}})
	plan := h.expect(h.do("POST", "/api/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + hash}, "destination_ref": "default"}), 201)
	id := h.expect(h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref")}), 201).str("id")
	h.fake.Tick(0)
	h.svc.Reconcile(context.Background())
	if v := h.task(id); v.str("hold_reason") != "quota_exceeded" || v.str("state") != "paused" {
		t.Fatalf("quota hold: %s", v.raw)
	}
	if r := h.do("POST", "/api/v1/torrents/"+id+"/resume", nil); r.errCode() != "quota_exceeded" {
		t.Fatalf("resume over quota: %s", r.raw)
	}
	// auto_start=false waits for an explicit resume.
	id2 := h.create(makeTorrent("Wait", map[string]int{"w.mkv": 10}), "all", false)
	if v := h.task(id2); v.str("hold_reason") != "awaiting_start" || v.str("state") != "paused" {
		t.Fatal(string(v.raw))
	}
	if r := h.expect(h.do("POST", "/api/v1/torrents/"+id2+"/resume", nil), 200); r.str("hold_reason") != "" {
		t.Fatal(string(r.raw))
	}
	// Disk watermark: stops downloads, resumes them when space returns.
	id3 := h.create(makeTorrent("Disk", map[string]int{"d.mkv": 10}), "all", true)
	h.free = 10
	h.svc.Reconcile(context.Background())
	if v := h.task(id3); v.str("hold_reason") != "disk_low" || v.str("state") != "paused" {
		t.Fatalf("disk hold: %s", v.raw)
	}
	h.free = 100 << 30
	h.svc.Reconcile(context.Background())
	if v := h.task(id3); v.str("hold_reason") != "" || v.str("state") == "paused" {
		t.Fatalf("disk resume: %s", v.raw)
	}
}

func TestEngineOutage(t *testing.T) {
	h := newHarness(t)
	id := h.create(makeTorrent("O", map[string]int{"o.mkv": 10}), "all", true)
	h.fake.Set(func(f *fakeqb.Fake) { f.Down = true })
	if r := h.do("GET", "/api/v1/torrents", nil); r.status != 503 || r.errCode() != "engine_unavailable" {
		t.Fatalf("list while down: %d %s", r.status, r.raw)
	}
	if r := h.do("GET", "/api/readyz", nil); r.status != 503 {
		t.Fatal("readyz should fail")
	}
	h.fake.Set(func(f *fakeqb.Fake) { f.Down = false })
	h.fake.ExpireSessions() // engine restarted: SID invalid, adapter re-logs in
	if v := h.task(id); v.str("id") != id {
		t.Fatal(string(v.raw))
	}
}

func TestPauseUnconfirmed(t *testing.T) {
	h := newHarness(t)
	id := h.create(makeTorrent("P", map[string]int{"p.mkv": 10}), "all", true)
	h.fake.Set(func(f *fakeqb.Fake) { f.IgnoreStop = true })
	if r := h.do("POST", "/api/v1/torrents/"+id+"/pause", nil); r.status != 504 || r.errCode() != "engine_state_unconfirmed" {
		t.Fatalf("unconfirmed pause: %d %s", r.status, r.raw)
	}
}

func TestInputValidation(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.MaxTorrentFileBytes = 300 })
	if r := h.do("POST", "/api/v1/torrent-inputs", bytes.Repeat([]byte("a"), 400)); r.status != 413 {
		t.Fatalf("size limit: %d", r.status)
	}
	if r := h.do("POST", "/api/v1/torrent-inputs", []byte("garbage")); r.errCode() != "invalid_torrent" {
		t.Fatal(string(r.raw))
	}
	cases := []map[string]any{
		{"source": map[string]any{"type": "url", "magnet": "http://x"}, "destination_ref": "default"},
		{"source": map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40), "input_ref": "x"}, "destination_ref": "default"},
		{"source": map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, "destination_ref": "../etc"},
		{"source": map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, "destination_ref": "default", "save_path": "/tmp"},
	}
	for i, c := range cases {
		if r := h.do("POST", "/api/v1/torrent-plans", c); r.status < 400 {
			t.Errorf("case %d accepted: %s", i, r.raw)
		}
	}
	if r := h.do("POST", "/api/v1/torrent-plans", "x", "Content-Type", "text/plain"); r.status != 415 {
		t.Errorf("content type: %d", r.status)
	}
}

func TestRateLimit(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Limits.RequestsPerMinute = 3 })
	for range 3 {
		h.expect(h.do("GET", "/api/v1/capabilities", nil), 200)
	}
	if r := h.do("GET", "/api/v1/capabilities", nil); r.status != 429 || r.header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", r.status)
	}
}

func TestDisallowedTrackersAreDroppedNotFatal(t *testing.T) {
	h := newHarness(t)
	h.svc.SetPolicy(torrents.Policy{TrackerSchemes: []string{"http", "https", "udp"}})
	hash := strings.Repeat("ef", 20)
	magnet := "magnet:?xt=urn:btih:" + hash + "&dn=Test" +
		"&tr=udp%3A%2F%2F93.184.216.34%3A6969%2Fannounce" + // public literal: kept
		"&tr=udp%3A%2F%2F127.0.0.1%3A6969%2Fannounce" + // like a dead tracker resolving to loopback
		"&tr=http%3A%2F%2F192.168.1.9%2Fannounce" +
		"&tr=wss%3A%2F%2F93.184.216.34%2Fannounce" // unsupported scheme
	plan := h.expect(h.do("POST", "/api/v1/torrent-plans", map[string]any{
		"source": map[string]any{"type": "magnet", "magnet": magnet}, "destination_ref": "default",
	}), 201)
	removed := plan.body["removed_addresses"].(map[string]any)
	if removed["private_or_local_address"].(float64) != 2 || removed["unsupported_address"].(float64) != 1 ||
		plan.body["trackers"].(float64) != 1 || !strings.Contains(string(plan.raw), "addresses_removed") {
		t.Fatalf("plan: %s", plan.raw)
	}
	h.expect(h.do("POST", "/api/v1/torrents", map[string]any{"plan_ref": plan.str("plan_ref")}), 201)
	sent := h.fake.AddURLs()
	if !strings.Contains(sent, "93.184.216.34%3A6969") || strings.Contains(sent, "127.0.0.1") || strings.Contains(sent, "192.168.1.9") || strings.Contains(sent, "wss") {
		t.Fatalf("engine received %s", sent)
	}
	// Only a magnet with every tracker removed still works (DHT only).
	plan = h.expect(h.do("POST", "/api/v1/torrent-plans", map[string]any{
		"source":          map[string]any{"type": "magnet", "magnet": "magnet:?xt=urn:btih:" + strings.Repeat("cd", 20) + "&tr=udp%3A%2F%2F127.0.0.1%3A1%2Fa"},
		"destination_ref": "default",
	}), 201)
	if !strings.Contains(string(plan.raw), "no_trackers_dht_only") {
		t.Fatalf("dht-only warning: %s", plan.raw)
	}
}
