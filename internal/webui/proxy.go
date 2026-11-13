// Package webui serves the engine's native qBittorrent WebUI through
// bittorrent-server. Users still log in with the engine's own WebUI account;
// the proxy adds what the engine cannot know about this deployment:
//
//   - same-origin (CSRF) checks against the address the browser used, then
//     Host/Origin/Referer rewritten to the engine's private address so the
//     engine's own Host and CSRF validation stay enabled;
//   - endpoints that change global settings, shut down, install search
//     plugins, auto-download RSS or read arbitrary files are refused;
//   - adds accept magnets and torrent files only (no remote URL fetches) and
//     every save path stays below the download root, outside the API's
//     destinations;
//   - login failures are limited per real client IP (the engine's own ban is
//     disabled because all browsers share the proxy's address).
package webui

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

type Proxy struct {
	target   *url.URL
	root     string   // download root, engine path semantics
	reserved []string // API destination directories
	log      *slog.Logger
	rp       *httputil.ReverseProxy
	logins   *loginLimiter
}

const maxBody = 32 << 20

func New(engineURL, downloadRoot string, reserved []string, failuresPerClient int, log *slog.Logger) (*Proxy, error) {
	target, err := url.Parse(strings.TrimRight(engineURL, "/"))
	if err != nil || target.Host == "" {
		return nil, errors.New("webui: invalid engine url")
	}
	p := &Proxy{
		target: target, root: path.Clean(downloadRoot), log: log,
		logins: newLoginLimiter(failuresPerClient),
	}
	for _, r := range reserved {
		p.reserved = append(p.reserved, path.Clean(r))
	}
	origin := target.Scheme + "://" + target.Host
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", origin)
			}
			if ref, err := url.Parse(pr.In.Header.Get("Referer")); err == nil && ref.Host != "" {
				pr.Out.Header.Set("Referer", origin+ref.RequestURI())
			}
			// The API's bearer token never reaches the engine.
			pr.Out.Header.Del("Authorization")
		},
		ModifyResponse: func(res *http.Response) error {
			if res.Request.URL.Path != "/api/v2/auth/login" {
				return nil
			}
			body, err := io.ReadAll(io.LimitReader(res.Body, 4096))
			res.Body.Close()
			if err != nil {
				return err
			}
			res.Body = io.NopCloser(bytes.NewReader(body))
			if ip, _ := res.Request.Context().Value(clientKey{}).(string); ip != "" {
				p.logins.record(ip, strings.TrimSpace(string(body)) == "Ok.")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "qBittorrent engine unavailable", http.StatusBadGateway)
		},
	}
	return p, nil
}

type clientKey struct{}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sameOrigin mirrors the engine's CSRF rule against the browser-facing host:
// a present Origin or Referer must name the host the request was sent to.
func sameOrigin(r *http.Request) bool {
	for _, h := range []string{"Origin", "Referer"} {
		v := r.Header.Get(h)
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

// blocked endpoints: global settings, process control, search plugins,
// RSS auto-download, and the torrent creator (reads any engine-visible file).
func blocked(endpoint string) bool {
	switch endpoint {
	case "app/setPreferences", "app/shutdown", "app/sendTestEmail", "app/setCookies":
		return true
	}
	for _, prefix := range []string{"search/", "rss/", "torrentcreator/"} {
		if strings.HasPrefix(endpoint, prefix) {
			return true
		}
	}
	return false
}

// Path parameters that set where data is written.
var pathParams = map[string][]string{
	"torrents/add":             {"savepath", "downloadPath"},
	"torrents/setLocation":     {"location"},
	"torrents/setSavePath":     {"path"},
	"torrents/setDownloadPath": {"path"},
	"torrents/createCategory":  {"savePath", "downloadPath"},
	"torrents/editCategory":    {"savePath", "downloadPath"},
}

// allowedPath: below the download root, never inside an API destination.
// Relative paths resolve below the engine's default save path, which is
// itself under the root.
func (p *Proxy) allowedPath(v string) bool {
	if v == "" {
		return true
	}
	if strings.ContainsAny(v, "\\\x00") {
		return false
	}
	clean := path.Clean(v)
	if !path.IsAbs(clean) {
		return clean != ".." && !strings.HasPrefix(clean, "../")
	}
	if clean != p.root && !strings.HasPrefix(clean, p.root+"/") {
		return false
	}
	for _, r := range p.reserved {
		if clean == r || strings.HasPrefix(clean, r+"/") {
			return false
		}
	}
	return true
}

type denial struct {
	status int
	msg    string
}

func (p *Proxy) inspect(endpoint string, form url.Values) *denial {
	if endpoint == "torrents/add" {
		for _, line := range strings.FieldsFunc(form.Get("urls"), func(r rune) bool { return r == '\n' || r == '\r' }) {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(strings.ToLower(line), "magnet:?") {
				return &denial{http.StatusForbidden, "Only magnet links and .torrent files can be added here."}
			}
		}
	}
	for _, key := range pathParams[endpoint] {
		if !p.allowedPath(form.Get(key)) {
			return &denial{http.StatusForbidden, "Save paths must stay inside the download root and outside OpenPlayX-managed folders."}
		}
	}
	return nil
}

// readForm buffers the body (so it can be forwarded unchanged) and parses
// urlencoded or multipart fields; files are ignored.
func readForm(r *http.Request) (url.Values, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body.Close()
	if err != nil || len(body) > maxBody {
		return nil, errors.New("body too large")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	form := url.Values{}
	if q, err := url.ParseQuery(r.URL.RawQuery); err == nil {
		for k, v := range q {
			form[k] = append(form[k], v...)
		}
	}
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mt {
	case "application/x-www-form-urlencoded":
		q, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		for k, v := range q {
			form[k] = append(form[k], v...)
		}
	case "multipart/form-data":
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if part.FileName() == "" {
				v, _ := io.ReadAll(io.LimitReader(part, 1<<20))
				form.Add(part.FormName(), string(v))
			}
			part.Close()
		}
	}
	return form, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "Cross-origin request refused", http.StatusForbidden)
		return
	}
	ip := clientIP(r)
	if endpoint, ok := strings.CutPrefix(r.URL.Path, "/api/v2/"); ok {
		if blocked(endpoint) {
			p.log.Info("webui endpoint blocked", "endpoint", endpoint)
			http.Error(w, "This function is disabled on the OpenPlayX BitTorrent Server.", http.StatusForbidden)
			return
		}
		if endpoint == "auth/login" && !p.logins.allowed(ip) {
			w.Header().Set("Retry-After", "900")
			http.Error(w, "Too many failed logins; try again later.", http.StatusTooManyRequests)
			return
		}
		if r.Method == http.MethodPost {
			form, err := readForm(r)
			if err != nil {
				http.Error(w, "Bad request", http.StatusBadRequest)
				return
			}
			if d := p.inspect(endpoint, form); d != nil {
				p.log.Info("webui request refused", "endpoint", endpoint, "status", d.status)
				http.Error(w, d.msg, d.status)
				return
			}
		}
	}
	r = r.WithContext(contextWith(r, ip))
	p.rp.ServeHTTP(w, r)
}

// loginLimiter counts failed WebUI logins per client IP over a sliding
// 15-minute window. There is deliberately no global counter: it would let
// one client lock everyone out.
type loginLimiter struct {
	mu        sync.Mutex
	perClient int
	failures  map[string][]time.Time
}

const loginWindow = 15 * time.Minute

func newLoginLimiter(perClient int) *loginLimiter {
	if perClient <= 0 {
		perClient = 5
	}
	return &loginLimiter{perClient: perClient, failures: map[string][]time.Time{}}
}

func prune(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > loginWindow {
		i++
	}
	return ts[i:]
}

func (l *loginLimiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.failures[ip] = prune(l.failures[ip], now)
	return len(l.failures[ip]) < l.perClient
}

func (l *loginLimiter) record(ip string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ok {
		delete(l.failures, ip)
		return
	}
	now := time.Now()
	l.failures[ip] = append(l.failures[ip], now)
	if len(l.failures) > 10000 {
		l.failures = map[string][]time.Time{}
	}
}
