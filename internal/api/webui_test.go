package api_test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/openplayx/bittorrent-server/internal/config"
	"github.com/openplayx/bittorrent-server/test/fakeqb"
)

type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) post(path string, form url.Values, origin string) (int, string) {
	b.t.Helper()
	req, _ := http.NewRequest("POST", b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin == "" {
		origin = b.base
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func (b *browser) postMultipart(path string, fields map[string]string) (int, string) {
	b.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	req, _ := http.NewRequest("POST", b.base+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", b.base)
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func (b *browser) get(path string) (int, string, http.Header) {
	b.t.Helper()
	res, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body), res.Header
}

func TestWebUIProxy(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.WebUI.Enabled = true })
	b := newBrowser(t, h.srv.URL)

	status, body, _ := b.get("/")
	if status != 200 || !strings.Contains(body, "qBittorrent WebUI") {
		t.Fatalf("index: %d %s", status, body)
	}
	// The engine sees its own private host, so its Host check stays on.
	if !strings.Contains(body, "host="+strings.TrimPrefix(h.fake.Server.URL, "http://")) {
		t.Fatalf("host not rewritten: %s", body)
	}
	// The service API keeps working next to the WebUI.
	h.expect(h.do("GET", "/api/v1/capabilities", nil), 200)

	if s, _ := b.post("/api/v2/auth/login", url.Values{"username": {"admin"}, "password": {"secret"}}, "http://evil.example"); s != 403 {
		t.Fatalf("cross-origin login: %d", s)
	}
	if s, body := b.post("/api/v2/auth/login", url.Values{"username": {"admin"}, "password": {"secret"}}, ""); s != 200 || body != "Ok." {
		t.Fatalf("login: %d %s", s, body)
	}
	if s, _, _ := b.get("/api/v2/app/version"); s != 200 {
		t.Fatalf("session cookie not forwarded: %d", s)
	}
	for _, ep := range []string{"app/setPreferences", "app/shutdown", "search/installPlugin", "rss/addFeed", "torrentcreator/addTask"} {
		if s, _ := b.post("/api/v2/"+ep, url.Values{"json": {"{}"}}, ""); s != 403 {
			t.Errorf("%s not blocked: %d", ep, s)
		}
	}
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("ef", 20)
	refusals := []url.Values{
		{"urls": {"http://192.168.1.1/x.torrent"}},
		{"urls": {magnet}, "savepath": {"/etc"}},
		{"urls": {magnet}, "savepath": {h.cfg.DownloadRoot + "/test/tsk_x"}}, // API destination
		{"urls": {magnet}, "savepath": {"../../config"}},
	}
	for _, f := range refusals {
		fields := map[string]string{}
		for k, v := range f {
			fields[k] = v[0]
		}
		if s, _ := b.postMultipart("/api/v2/torrents/add", fields); s != 403 {
			t.Errorf("add %v allowed: %d", f, s)
		}
	}
	if s, body := b.postMultipart("/api/v2/torrents/add", map[string]string{"urls": magnet, "savepath": h.cfg.DownloadRoot + "/webui"}); s != 200 || body != "Ok." {
		t.Fatalf("allowed add: %d %s", s, body)
	}

	// No ownership split any more: the WebUI can control torrents the App
	// added, and the API sees WebUI changes live.
	id := h.create(makeTorrent("FromApp", map[string]int{"m.mkv": 10}), "all", true)
	if s, _ := b.post("/api/v2/torrents/stop", url.Values{"hashes": {id}}, ""); s != 200 {
		t.Fatalf("webui stop of app torrent: %d", s)
	}
	if v := h.task(id); v.str("state") != "paused" {
		t.Fatalf("api does not see webui change: %s", v.raw)
	}
	if s, _ := b.post("/api/v2/torrents/delete", url.Values{"hashes": {strings.Repeat("ef", 20)}}, ""); s != 200 {
		t.Fatalf("unmanaged delete: %d", s)
	}
	if _, ok := h.fake.Get(strings.Repeat("ef", 20)); ok {
		t.Fatal("unmanaged torrent not deleted")
	}

	// Login failures are limited per client, independent of the engine.
	h.fake.Set(func(f *fakeqb.Fake) {})
	for i := 0; i < 5; i++ {
		if _, body := b.post("/api/v2/auth/login", url.Values{"username": {"admin"}, "password": {"bad"}}, ""); body != "Fails." {
			t.Fatalf("failure %d: %s", i, body)
		}
	}
	if s, _ := b.post("/api/v2/auth/login", url.Values{"username": {"admin"}, "password": {"secret"}}, ""); s != 429 {
		t.Fatalf("limiter: %d", s)
	}
}

func TestWebUIDisabled(t *testing.T) {
	h := newHarness(t)
	b := newBrowser(t, h.srv.URL)
	if s, _, hdr := b.get("/"); s != 302 || hdr.Get("Location") != "/api/docs" {
		t.Fatalf("root without webui: %d %v", s, hdr)
	}
	if s, _, _ := b.get("/api/v2/app/version"); s != 404 {
		t.Fatalf("engine api exposed without webui: %d", s)
	}
}
