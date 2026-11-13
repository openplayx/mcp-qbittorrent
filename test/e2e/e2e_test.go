//go:build e2e

// End-to-end suite against real qbittorrent-nox containers. Run through
// scripts/e2e.sh, which builds images, generates credentials and starts the
// isolated compose stack.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openplayx/bittorrent-server/internal/torrents"
)

var (
	apiURL    = os.Getenv("E2E_API")
	token     = os.Getenv("E2E_TOKEN")
	seederURL = os.Getenv("E2E_SEEDER")
	work      = os.Getenv("WORK")
	compose   = os.Getenv("E2E_COMPOSE")
	secrets   = strings.Split(os.Getenv("E2E_SECRETS"), ",")
)

type report struct {
	Started  time.Time         `json:"started"`
	Engine   map[string]any    `json:"engine"`
	Cases    map[string]string `json:"cases"`
	Fixtures map[string]string `json:"fixtures_sha256"`
}

var rep = report{Cases: map[string]string{}, Fixtures: map[string]string{}}

// ---------- API client ----------

type res struct {
	status int
	body   map[string]any
	raw    []byte
	hdr    http.Header
}

func (r res) s(k string) string { v, _ := r.body[k].(string); return v }
func (r res) code() string {
	if e, ok := r.body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func call(t *testing.T, method, path string, body any, hdr ...string) res {
	t.Helper()
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
	req, _ := http.NewRequest(method, apiURL+path, rd)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := res{status: resp.StatusCode, raw: raw, hdr: resp.Header}
	_ = json.Unmarshal(raw, &r.body)
	if !strings.Contains(path, "/content") {
		for _, sec := range secrets {
			if sec != "" && bytes.Contains(raw, []byte(sec)) {
				t.Fatalf("response to %s leaks a secret", path)
			}
		}
		for _, leak := range []string{"/downloads", "magnet:?", "SID="} {
			if bytes.Contains(raw, []byte(leak)) {
				t.Fatalf("response to %s leaks %q: %s", path, leak, raw)
			}
		}
	}
	return r
}

func must(t *testing.T, r res, status int) res {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d want %d: %s", r.status, status, r.raw)
	}
	return r
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(d)
	last := ""
	for time.Now().Before(deadline) {
		ok, info := cond()
		if ok {
			return
		}
		last = info
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s; last: %s", what, last)
}

func waitOp(t *testing.T, r res) res {
	t.Helper()
	if r.status != 200 && r.status != 202 {
		t.Fatalf("operation: %d %s", r.status, r.raw)
	}
	for r.s("status") == "pending" {
		time.Sleep(300 * time.Millisecond)
		r = must(t, call(t, "GET", "/v1/operations/"+r.s("id"), nil), 200)
	}
	if r.s("status") != "succeeded" {
		t.Fatalf("operation failed: %s", r.raw)
	}
	return r
}

func task(t *testing.T, id string) res { return must(t, call(t, "GET", "/v1/torrents/"+id, nil), 200) }

func waitState(t *testing.T, id string, d time.Duration, cond func(r res) bool) res {
	t.Helper()
	var last res
	waitFor(t, "task "+id, d, func() (bool, string) {
		last = task(t, id)
		return cond(last), string(last.raw)
	})
	return last
}

func complete(r res) bool {
	return r.body["selected_files_complete"] == true && r.body["stale"] == false
}

// ---------- seeder (fixture engine) client ----------

type qb struct {
	base string
	c    *http.Client
}

func (q *qb) post(t *testing.T, path string, form url.Values) []byte {
	t.Helper()
	req, _ := http.NewRequest("POST", q.base+"/api/v2/"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", q.base+"/")
	resp, err := q.c.Do(req)
	if err != nil {
		t.Fatalf("seeder %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("seeder %s: %d %s", path, resp.StatusCode, raw)
	}
	return raw
}

func seeder(t *testing.T) *qb {
	jar, _ := cookiejar.New(nil)
	q := &qb{base: seederURL, c: &http.Client{Jar: jar, Timeout: 20 * time.Second}}
	waitFor(t, "seeder webui", 60*time.Second, func() (bool, string) {
		req, _ := http.NewRequest("POST", q.base+"/api/v2/auth/login", strings.NewReader(url.Values{"username": {"seed"}, "password": {os.Getenv("SEEDER_PASS")}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Referer", q.base+"/")
		resp, err := q.c.Do(req)
		if err != nil {
			return false, err.Error()
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(raw) == "Ok.", string(raw)
	})
	prefs, _ := json.Marshal(map[string]any{"enable_embedded_tracker": true, "embedded_tracker_port": 9000})
	q.post(t, "app/setPreferences", url.Values{"json": {string(prefs)}})
	return q
}

// createTorrent makes the seeder build and seed a torrent from /seed/<src>.
func (q *qb) createTorrent(t *testing.T, src, format string, private bool) []byte {
	t.Helper()
	raw := q.post(t, "torrentcreator/addTask", url.Values{
		"sourcePath": {"/seed/" + src}, "format": {format}, "pieceSize": {"32768"},
		"private": {fmt.Sprint(private)}, "startSeeding": {"true"},
		"trackers": {"http://seeder:9000/announce"},
	})
	var created struct {
		TaskID string `json:"taskID"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.TaskID == "" {
		t.Fatalf("torrentcreator: %s", raw)
	}
	waitFor(t, "torrent creation "+src, 60*time.Second, func() (bool, string) {
		var st []map[string]any
		_ = json.Unmarshal(q.post(t, "torrentcreator/status", url.Values{"taskID": {created.TaskID}}), &st)
		if len(st) == 1 && st[0]["status"] == "Failed" {
			t.Fatalf("creation failed: %v", st)
		}
		return len(st) == 1 && st[0]["status"] == "Finished", fmt.Sprint(st)
	})
	return q.post(t, "torrentcreator/torrentFile", url.Values{"taskID": {created.TaskID}})
}

func (q *qb) waitSeeding(t *testing.T, n int) {
	waitFor(t, "seeder seeding all fixtures", 90*time.Second, func() (bool, string) {
		var list []map[string]any
		_ = json.Unmarshal(q.post(t, "torrents/info", url.Values{}), &list)
		seeding := 0
		states := []string{}
		for _, x := range list {
			st := x["state"].(string)
			states = append(states, st)
			if st == "uploading" || st == "stalledUP" {
				seeding++
			}
		}
		return seeding >= n, strings.Join(states, ",")
	})
}

// ---------- fixtures ----------

func writeFixture(t *testing.T, rel string, size int, seed uint64) {
	t.Helper()
	p := filepath.Join(work, "seed", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	r := mrand.New(mrand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(r.Uint32())
	}
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf)
	rep.Fixtures[rel] = hex.EncodeToString(sum[:])
}

func downloadedSHA(t *testing.T, taskID, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(work, "downloads", "e2e", taskID, rel))
	if err != nil {
		t.Fatalf("read downloaded %s: %v", rel, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func verifyFiles(t *testing.T, taskID, srcDir string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		// The torrent root folder is the source dir name; a single-file
		// torrent is just the file.
		want, got := rep.Fixtures[rel], ""
		if srcDir == rel {
			got = downloadedSHA(t, taskID, rel)
		} else {
			want = rep.Fixtures[srcDir+"/"+rel]
			got = downloadedSHA(t, taskID, filepath.Base(srcDir)+"/"+rel)
		}
		if got != want {
			t.Fatalf("%s/%s sha256 %s want %s", srcDir, rel, got, want)
		}
	}
}

func createTask(t *testing.T, torrent []byte, preset string, autoStart bool, key string) (string, res) {
	t.Helper()
	in := must(t, call(t, "POST", "/v1/torrent-inputs", torrent), 201)
	plan := must(t, call(t, "POST", "/v1/torrent-plans", map[string]any{
		"source": map[string]any{"type": "input", "input_ref": in.s("input_ref")}, "destination_ref": "default",
		"selection_preset": preset, "auto_start": autoStart,
	}), 201)
	body := map[string]any{"plan_ref": plan.s("plan_ref"), "expected_revision": 1, "idempotency_key": key}
	op := waitOp(t, call(t, "POST", "/v1/torrents", body))
	return op.s("task_id"), op
}

func dc(t *testing.T, args ...string) {
	t.Helper()
	parts := append(strings.Fields(compose), args...)
	out, err := exec.Command(parts[0], parts[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("compose %v: %v %s", args, err, out)
	}
}

func waitReady(t *testing.T) {
	waitFor(t, "api ready", 90*time.Second, func() (bool, string) {
		resp, err := http.Get(apiURL + "/readyz")
		if err != nil {
			return false, err.Error()
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode == 200, string(raw)
	})
}

func pass(t *testing.T, name string, f func(t *testing.T)) {
	ok := t.Run(name, f)
	rep.Cases[name] = map[bool]string{true: "pass", false: "fail"}[ok]
	if !ok {
		t.FailNow()
	}
}

// ---------- the suite ----------

func TestEndToEnd(t *testing.T) {
	if apiURL == "" {
		t.Skip("run via scripts/e2e.sh")
	}
	rep.Started = time.Now().UTC()
	defer func() {
		raw, _ := json.MarshalIndent(rep, "", "  ")
		_ = os.WriteFile(filepath.Join(work, "report.json"), raw, 0o644)
	}()
	waitReady(t)

	pass(t, "health_auth_docs", func(t *testing.T) {
		for _, p := range []string{"/healthz", "/readyz", "/openapi.json", "/docs"} {
			resp, err := http.Get(apiURL + p)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("%s: %v %v", p, err, resp)
			}
			resp.Body.Close()
		}
		if r := call(t, "GET", "/v1/capabilities", nil, "Authorization", "Bearer wrong"); r.status != 401 {
			t.Fatalf("bad token accepted: %d", r.status)
		}
		caps := must(t, call(t, "GET", "/v1/capabilities", nil), 200)
		rep.Engine = caps.body["engine"].(map[string]any)
		if rep.Engine["version"] != "v5.1.0" || rep.Engine["available"] != true {
			t.Fatalf("engine: %s", caps.raw)
		}
	})

	q := seeder(t)
	writeFixture(t, "clip.mkv", 3<<20+123, 1)
	writeFixture(t, "multi/a.mkv", 2<<20, 2)
	writeFixture(t, "multi/b.mkv", 1<<20+777, 3)
	writeFixture(t, "multi/notes.txt", 10_000, 4)
	writeFixture(t, "multi/sub/c.srt", 5_000, 5)
	writeFixture(t, "selective/x.mkv", 2<<20, 6)
	writeFixture(t, "selective/y.mkv", 2<<20, 7)
	writeFixture(t, "selective/z.txt", 50_000, 8)
	writeFixture(t, "magnetsrc/m.mkv", 1<<20, 9)
	writeFixture(t, "magnetsrc/sample.mkv", 100_000, 10)
	writeFixture(t, "magnetsrc/m.nfo", 300, 11)
	single := q.createTorrent(t, "clip.mkv", "v1", true)
	multiV1 := q.createTorrent(t, "multi", "v1", true)
	multiV2 := q.createTorrent(t, "multi", "v2", true)
	multiHy := q.createTorrent(t, "multi", "hybrid", true)
	selective := q.createTorrent(t, "selective", "v1", true)
	magnetSrc := q.createTorrent(t, "magnetsrc", "hybrid", false)
	q.waitSeeding(t, 6)
	// Keep the fixtures' .torrent files for debugging and App-side tests.
	_ = os.MkdirAll(filepath.Join(work, "torrents"), 0o755)
	for name, data := range map[string][]byte{"single-v1": single, "multi-v1": multiV1, "multi-v2": multiV2, "multi-hybrid": multiHy, "selective-v1": selective, "magnet-hybrid": magnetSrc} {
		_ = os.WriteFile(filepath.Join(work, "torrents", name+".torrent"), data, 0o644)
	}

	var singleID, singleOp string
	multiIDs := map[string]string{}

	pass(t, "torrent_file_add_and_integrity", func(t *testing.T) {
		singleID, _ = createTask(t, single, "all", true, "single-1")
		r := waitState(t, singleID, 120*time.Second, complete)
		if r.body["payload_complete"] != true || r.s("torrent_version") != "v1" {
			t.Fatalf("single: %s", r.raw)
		}
		verifyFiles(t, singleID, "clip.mkv", "clip.mkv")
	})

	pass(t, "idempotent_create", func(t *testing.T) {
		// Re-plan is refused as duplicate; the original key replays the operation.
		in := must(t, call(t, "POST", "/v1/torrent-inputs", single), 201)
		r := call(t, "POST", "/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "input", "input_ref": in.s("input_ref")}, "destination_ref": "default"})
		if r.code() != "duplicate_task" {
			t.Fatalf("duplicate plan: %s", r.raw)
		}
		list := must(t, call(t, "GET", "/v1/torrents", nil), 200)
		if n := len(list.body["items"].([]any)); n != 1 {
			t.Fatalf("tasks: %d", n)
		}
		_ = singleOp
	})

	pass(t, "v1_v2_hybrid_multi_file", func(t *testing.T) {
		for name, data := range map[string][]byte{"v1": multiV1, "v2": multiV2, "hybrid": multiHy} {
			id, _ := createTask(t, data, "all", true, "multi-"+name)
			multiIDs[name] = id
		}
		for name, id := range multiIDs {
			r := waitState(t, id, 180*time.Second, complete)
			if r.s("torrent_version") != name || r.body["payload_complete"] != true {
				t.Fatalf("%s: %s", name, r.raw)
			}
			verifyFiles(t, id, "multi", "a.mkv", "b.mkv", "notes.txt", "sub/c.srt")
		}
	})

	pass(t, "file_selection", func(t *testing.T) {
		id, _ := createTask(t, selective, "all", false, "selective")
		r := waitState(t, id, 60*time.Second, func(r res) bool {
			return r.s("hold_reason") == "awaiting_selection" && r.body["metadata_known"] == true
		})
		files := must(t, call(t, "GET", "/v1/torrents/"+id+"/files", nil), 200)
		var x string
		for _, f := range files.body["files"].([]any) {
			fm := f.(map[string]any)
			if strings.HasSuffix(fm["path"].(string), "x.mkv") {
				x = fm["file_id"].(string)
			}
		}
		rev := int(r.body["files_revision"].(float64))
		waitOp(t, call(t, "PATCH", "/v1/torrents/"+id+"/selection", map[string]any{"expected_revision": rev, "file_ids": []string{x}}))
		waitOp(t, call(t, "POST", "/v1/torrents/"+id+"/resume", nil))
		r = waitState(t, id, 120*time.Second, complete)
		if r.body["payload_complete"] == true {
			t.Fatalf("partial selection claims payload complete: %s", r.raw)
		}
		files = must(t, call(t, "GET", "/v1/torrents/"+id+"/files", nil), 200)
		for _, f := range files.body["files"].([]any) {
			fm := f.(map[string]any)
			isX := strings.HasSuffix(fm["path"].(string), "x.mkv")
			if fm["selected"] != isX || fm["complete"] != isX {
				t.Fatalf("selection state wrong: %v", fm)
			}
		}
		verifyFiles(t, id, "selective", "x.mkv")
		if r := call(t, "GET", "/v1/torrents/"+id+"/files/f1/content", nil); r.status == 200 {
			t.Fatal("unselected file served")
		}
	})

	pass(t, "magnet_metadata_discovery", func(t *testing.T) {
		in, err := torrents.ParseTorrent(context.Background(), magnetSrc, torrents.Policy{AllowPrivate: true, TrackerSchemes: []string{"http"}})
		if err != nil {
			t.Fatal(err)
		}
		magnet := "magnet:?xt=urn:btih:" + in.Identity.V1 + "&xt=urn:btmh:1220" + in.Identity.V2 + "&dn=magnetsrc&tr=" + url.QueryEscape("http://seeder:9000/announce")
		plan := must(t, call(t, "POST", "/v1/torrent-plans", map[string]any{
			"source": map[string]any{"type": "magnet", "magnet": magnet}, "destination_ref": "default", "selection_preset": "video",
		}), 201)
		if plan.s("metadata") != "unknown" || plan.s("torrent_version") != "hybrid" {
			t.Fatalf("plan: %s", plan.raw)
		}
		op := waitOp(t, call(t, "POST", "/v1/torrents", map[string]any{"plan_ref": plan.s("plan_ref"), "expected_revision": 1, "idempotency_key": "magnet"}))
		id := op.s("task_id")
		waitState(t, id, 120*time.Second, complete)
		files := must(t, call(t, "GET", "/v1/torrents/"+id+"/files", nil), 200)
		for _, f := range files.body["files"].([]any) {
			fm := f.(map[string]any)
			want := strings.HasSuffix(fm["path"].(string), "/m.mkv")
			if fm["selected"] != want {
				t.Fatalf("video preset selection wrong: %v", fm)
			}
		}
		verifyFiles(t, id, "magnetsrc", "m.mkv")
	})

	pass(t, "magnet_without_peers_cancellable", func(t *testing.T) {
		b := make([]byte, 20)
		_, _ = rand.Read(b)
		magnet := "magnet:?xt=urn:btih:" + hex.EncodeToString(b) + "&tr=" + url.QueryEscape("http://seeder:9000/announce")
		plan := must(t, call(t, "POST", "/v1/torrent-plans", map[string]any{"source": map[string]any{"type": "magnet", "magnet": magnet}, "destination_ref": "default"}), 201)
		id := waitOp(t, call(t, "POST", "/v1/torrents", map[string]any{"plan_ref": plan.s("plan_ref"), "expected_revision": 1, "idempotency_key": "nopeers"})).s("task_id")
		time.Sleep(4 * time.Second)
		if r := task(t, id); r.s("state") != "metadata_pending" || r.body["metadata_known"] == true {
			t.Fatalf("no-peer magnet: %s", r.raw)
		}
		rp := must(t, call(t, "POST", "/v1/torrents/"+id+"/remove-plan", map[string]any{"delete_files": true}), 201)
		waitOp(t, call(t, "POST", "/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": rp.s("plan_ref"), "idempotency_key": "rm-nopeers"}))
		if r := task(t, id); r.s("state") != "removed" {
			t.Fatal(string(r.raw))
		}
	})

	pass(t, "content_range_and_head", func(t *testing.T) {
		files := must(t, call(t, "GET", "/v1/torrents/"+singleID+"/files", nil), 200)
		f := files.body["files"].([]any)[0].(map[string]any)
		if f["content_available"] != true {
			t.Fatalf("content not available: %v", f)
		}
		path := "/v1/torrents/" + singleID + "/files/" + f["file_id"].(string) + "/content"
		r := call(t, "GET", path, nil, "Range", "bytes=1000-1999")
		src, _ := os.ReadFile(filepath.Join(work, "seed", "clip.mkv"))
		if r.status != 206 || !bytes.Equal(r.raw, src[1000:2000]) {
			t.Fatalf("range: %d len=%d", r.status, len(r.raw))
		}
		h := call(t, "HEAD", path, nil)
		if h.status != 200 || h.hdr.Get("Content-Length") != fmt.Sprint(len(src)) || h.hdr.Get("Accept-Ranges") != "bytes" {
			t.Fatalf("head: %d %v", h.status, h.hdr)
		}
		full := call(t, "GET", path, nil)
		sum := sha256.Sum256(full.raw)
		if hex.EncodeToString(sum[:]) != rep.Fixtures["clip.mkv"] {
			t.Fatal("content sha mismatch")
		}
	})

	pass(t, "pause_resume_limits", func(t *testing.T) {
		id := multiIDs["v1"]
		op := waitOp(t, call(t, "POST", "/v1/torrents/"+id+"/pause", map[string]any{"idempotency_key": "p1"}))
		if op.body["result"].(map[string]any)["state"] != "completed" {
			t.Fatalf("pause: %s", op.raw)
		}
		waitState(t, id, 15*time.Second, func(r res) bool { return r.s("state") == "completed" && r.body["seeding"] == false })
		op = waitOp(t, call(t, "POST", "/v1/torrents/"+id+"/resume", map[string]any{"idempotency_key": "r1"}))
		waitState(t, id, 15*time.Second, func(r res) bool { return r.s("state") == "seeding" })
		waitOp(t, call(t, "PATCH", "/v1/torrents/"+id+"/limits", map[string]any{"download_limit": 1 << 20, "upload_limit": 512 << 10}))
		if r := task(t, id); r.body["download_limit"].(float64) != 1<<20 || r.body["upload_limit"].(float64) != 512<<10 {
			t.Fatalf("limits: %s", r.raw)
		}
		if r := call(t, "PATCH", "/v1/torrents/"+id+"/limits", map[string]any{"download_limit": 100 << 20}); r.status != 422 {
			t.Fatalf("limit above ceiling: %d", r.status)
		}
	})

	pass(t, "restart_recovery", func(t *testing.T) {
		before := must(t, call(t, "GET", "/v1/torrents", nil), 200)
		dc(t, "restart", "qbittorrent", "api")
		waitReady(t)
		after := must(t, call(t, "GET", "/v1/torrents", nil), 200)
		if len(before.body["items"].([]any)) != len(after.body["items"].([]any)) {
			t.Fatal("task count changed across restart")
		}
		waitState(t, singleID, 60*time.Second, func(r res) bool { return complete(r) && r.s("state") != "error" })
	})

	pass(t, "engine_offline_stale", func(t *testing.T) {
		dc(t, "stop", "qbittorrent")
		waitState(t, singleID, 30*time.Second, func(r res) bool { return r.body["stale"] == true && r.s("engine_status") == "unavailable" })
		if resp, err := http.Get(apiURL + "/readyz"); err != nil || resp.StatusCode != 503 {
			t.Fatalf("readyz while engine down: %v", resp)
		}
		// Tasks are kept, not deleted, while the engine is away.
		if n := len(must(t, call(t, "GET", "/v1/torrents", nil), 200).body["items"].([]any)); n < 6 {
			t.Fatalf("tasks lost while offline: %d", n)
		}
		dc(t, "start", "qbittorrent")
		waitReady(t)
		waitState(t, singleID, 60*time.Second, func(r res) bool { return complete(r) })
	})

	pass(t, "remove_keeps_files", func(t *testing.T) {
		id := multiIDs["v2"]
		rp := must(t, call(t, "POST", "/v1/torrents/"+id+"/remove-plan", map[string]any{"delete_files": false}), 201)
		waitOp(t, call(t, "POST", "/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": rp.s("plan_ref"), "idempotency_key": "rm-v2"}))
		verifyFiles(t, id, "multi", "a.mkv", "b.mkv")
	})

	pass(t, "remove_deletes_only_task_files", func(t *testing.T) {
		id := multiIDs["hybrid"]
		rp := must(t, call(t, "POST", "/v1/torrents/"+id+"/remove-plan", map[string]any{"delete_files": true}), 201)
		if rp.body["bytes_on_disk"].(float64) <= 0 {
			t.Fatalf("plan: %s", rp.raw)
		}
		waitOp(t, call(t, "POST", "/v1/torrents/"+id+"/remove", map[string]any{"plan_ref": rp.s("plan_ref"), "idempotency_key": "rm-hy"}))
		if _, err := os.Stat(filepath.Join(work, "downloads", "e2e", id)); !os.IsNotExist(err) {
			t.Fatalf("hybrid dir still present: %v", err)
		}
		verifyFiles(t, multiIDs["v1"], "multi", "a.mkv")
		verifyFiles(t, multiIDs["v2"], "multi", "a.mkv")
		verifyFiles(t, singleID, "clip.mkv", "clip.mkv")
	})
}
