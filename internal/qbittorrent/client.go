// Package qbittorrent adapts the qBittorrent 5.x WebUI API. The SID cookie
// stays inside this package; callers only see normalised errors.
//
// Verified against qBittorrent v5.1.0 (WebAPI 2.11.x). 5.x renamed
// pause/resume to stop/start and the add parameter "paused" to "stopped";
// the adapter refuses to bind to versions outside the configured pin.
package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Error codes exposed to the application layer.
const (
	CodeUnavailable = "engine_unavailable"
	CodeAuth        = "engine_auth_failed"
	CodeRejected    = "engine_rejected"
	CodeNotFound    = "engine_not_found"
	CodeVersion     = "engine_version_unsupported"
)

type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string { return fmt.Sprintf("%s (status %d)", e.Code, e.Status) }

func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

type Client struct {
	base     *url.URL
	username string
	password string
	http     *http.Client

	mu       sync.Mutex
	loggedIn bool
	version  string
	webapi   string
}

func New(rawURL, username, password string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("qbittorrent: invalid engine url")
	}
	jar, _ := cookiejar.New(nil)
	return &Client{
		base:     u,
		username: username,
		password: password,
		http: &http.Client{
			Timeout: timeout,
			Jar:     jar,
			// The engine never needs to redirect us; refusing keeps the SID
			// from being replayed to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (c *Client) endpoint(path string) string { return c.base.String() + "/api/v2/" + path }

// newRequest sets Referer/Origin to the engine address so the engine's own
// CSRF and Host-header checks stay enabled.
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), body)
	if err != nil {
		return nil, err
	}
	origin := c.base.Scheme + "://" + c.base.Host
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("Origin", origin)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func (c *Client) login(ctx context.Context) error {
	form := url.Values{"username": {c.username}, "password": {c.password}}
	req, err := c.newRequest(ctx, http.MethodPost, "auth/login", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Code: CodeUnavailable}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "Ok." {
		return &Error{Code: CodeAuth, Status: resp.StatusCode}
	}
	c.loggedIn = true
	return nil
}

type call struct {
	method      string
	path        string
	form        url.Values
	body        []byte
	contentType string
}

// do performs a call, logging in lazily and once more after a 403 (expired
// SID). Network errors map to engine_unavailable.
func (c *Client) do(ctx context.Context, cl call) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		c.mu.Lock()
		if !c.loggedIn {
			if err := c.login(ctx); err != nil {
				c.mu.Unlock()
				return nil, err
			}
		}
		c.mu.Unlock()
		var body io.Reader
		ct := cl.contentType
		switch {
		case cl.body != nil:
			body = bytes.NewReader(cl.body)
		case cl.form != nil && cl.method == http.MethodPost:
			body = strings.NewReader(cl.form.Encode())
			ct = "application/x-www-form-urlencoded"
		}
		path := cl.path
		if cl.form != nil && cl.method == http.MethodGet {
			path += "?" + cl.form.Encode()
		}
		req, err := c.newRequest(ctx, cl.method, path, body, ct)
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, &Error{Code: CodeUnavailable}
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if rerr != nil {
			return nil, &Error{Code: CodeUnavailable}
		}
		switch {
		case resp.StatusCode == http.StatusForbidden && attempt == 0:
			c.mu.Lock()
			c.loggedIn = false
			c.mu.Unlock()
			continue
		case resp.StatusCode == http.StatusForbidden:
			return nil, &Error{Code: CodeAuth, Status: resp.StatusCode}
		case resp.StatusCode == http.StatusNotFound:
			return nil, &Error{Code: CodeNotFound, Status: resp.StatusCode}
		case resp.StatusCode >= 500:
			return nil, &Error{Code: CodeUnavailable, Status: resp.StatusCode}
		case resp.StatusCode != http.StatusOK:
			return nil, &Error{Code: CodeRejected, Status: resp.StatusCode}
		}
		return data, nil
	}
	return nil, &Error{Code: CodeAuth}
}

// Check binds the adapter to a pinned engine version.
func (c *Client) Check(ctx context.Context, supported []string, minWebAPI string) (string, string, error) {
	v, err := c.do(ctx, call{method: http.MethodGet, path: "app/version"})
	if err != nil {
		return "", "", err
	}
	w, err := c.do(ctx, call{method: http.MethodGet, path: "app/webapiVersion"})
	if err != nil {
		return "", "", err
	}
	version, webapi := strings.TrimSpace(string(v)), strings.TrimSpace(string(w))
	c.mu.Lock()
	c.version, c.webapi = version, webapi
	c.mu.Unlock()
	if !slices.Contains(supported, version) || compareVersion(webapi, minWebAPI) < 0 {
		return version, webapi, &Error{Code: CodeVersion}
	}
	return version, webapi, nil
}

func (c *Client) Versions() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version, c.webapi
}

func compareVersion(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// AddRequest uses only the parameters this service needs. There is no URL
// download, run-external-program or category/global-setting passthrough.
type AddRequest struct {
	Magnet        string
	Torrent       []byte
	SavePath      string
	Tag           string
	Stopped       bool
	StopCondition string // "", "MetadataReceived", "FilesChecked"
	DownloadLimit int64
	UploadLimit   int64
	RatioLimit    float64
	SeedingTime   int
}

// Add submits a torrent. An HTTP 200 does not prove the torrent exists; the
// caller confirms through Info.
func (c *Client) Add(ctx context.Context, r AddRequest) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	field := func(k, v string) { _ = mw.WriteField(k, v) }
	if r.Magnet != "" {
		field("urls", r.Magnet)
	}
	if r.Torrent != nil {
		fw, err := mw.CreateFormFile("torrents", "input.torrent")
		if err != nil {
			return err
		}
		_, _ = fw.Write(r.Torrent)
	}
	field("savepath", r.SavePath)
	field("autoTMM", "false")
	field("tags", r.Tag)
	field("stopped", strconv.FormatBool(r.Stopped))
	if r.StopCondition != "" {
		field("stopCondition", r.StopCondition)
	}
	field("contentLayout", "Original")
	if r.DownloadLimit > 0 {
		field("dlLimit", strconv.FormatInt(r.DownloadLimit, 10))
	}
	if r.UploadLimit > 0 {
		field("upLimit", strconv.FormatInt(r.UploadLimit, 10))
	}
	field("ratioLimit", strconv.FormatFloat(r.RatioLimit, 'f', -1, 64))
	field("seedingTimeLimit", strconv.Itoa(r.SeedingTime))
	if err := mw.Close(); err != nil {
		return err
	}
	data, err := c.do(ctx, call{method: http.MethodPost, path: "torrents/add", body: buf.Bytes(), contentType: mw.FormDataContentType()})
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(string(data)); s == "Fails." {
		return &Error{Code: CodeRejected, Status: http.StatusOK}
	}
	return nil
}

// Torrent is the subset of /torrents/info fields the service projects.
type Torrent struct {
	Hash         string  `json:"hash"`
	InfohashV1   string  `json:"infohash_v1"`
	InfohashV2   string  `json:"infohash_v2"`
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	Size         int64   `json:"size"`       // selected size
	TotalSize    int64   `json:"total_size"` // all files
	AmountLeft   int64   `json:"amount_left"`
	Completed    int64   `json:"completed"`
	Downloaded   int64   `json:"downloaded"`
	Uploaded     int64   `json:"uploaded"`
	DlSpeed      int64   `json:"dlspeed"`
	UpSpeed      int64   `json:"upspeed"`
	DlLimit      int64   `json:"dl_limit"`
	UpLimit      int64   `json:"up_limit"`
	NumSeeds     int     `json:"num_seeds"`
	NumLeechs    int     `json:"num_leechs"`
	Ratio        float64 `json:"ratio"`
	ETA          int64   `json:"eta"`
	SavePath     string  `json:"save_path"`
	Tags         string  `json:"tags"`
	HasMetadata  bool    `json:"has_metadata"`
	CompletionOn int64   `json:"completion_on"`
	AddedOn      int64   `json:"added_on"`
}

func (c *Client) Info(ctx context.Context, tag string) ([]Torrent, error) {
	form := url.Values{}
	if tag != "" {
		form.Set("tag", tag)
	}
	return c.info(ctx, form)
}

// InfoHash returns the torrent with this engine hash (empty if absent).
func (c *Client) InfoHash(ctx context.Context, hash string) ([]Torrent, error) {
	return c.info(ctx, url.Values{"hashes": {hash}})
}

// Ping is a cheap liveness/auth probe for readiness checks.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, call{method: http.MethodGet, path: "app/version"})
	return err
}

func (c *Client) info(ctx context.Context, form url.Values) ([]Torrent, error) {
	data, err := c.do(ctx, call{method: http.MethodGet, path: "torrents/info", form: form})
	if err != nil {
		return nil, err
	}
	var out []Torrent
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &Error{Code: CodeUnavailable}
	}
	return out, nil
}

type File struct {
	Index        int     `json:"index"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	Priority     int     `json:"priority"`
	Availability float64 `json:"availability"`
}

func (c *Client) Files(ctx context.Context, hash string) ([]File, error) {
	data, err := c.do(ctx, call{method: http.MethodGet, path: "torrents/files", form: url.Values{"hash": {hash}}})
	if err != nil {
		return nil, err
	}
	var out []File
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &Error{Code: CodeUnavailable}
	}
	return out, nil
}

// SetFilePriority sets priority (0 = skip, 1 = normal) for file indexes.
func (c *Client) SetFilePriority(ctx context.Context, hash string, indexes []int, priority int) error {
	if len(indexes) == 0 {
		return nil
	}
	ids := make([]string, len(indexes))
	for i, x := range indexes {
		ids[i] = strconv.Itoa(x)
	}
	_, err := c.do(ctx, call{method: http.MethodPost, path: "torrents/filePrio", form: url.Values{
		"hash": {hash}, "id": {strings.Join(ids, "|")}, "priority": {strconv.Itoa(priority)},
	}})
	return err
}

func (c *Client) hashesCall(ctx context.Context, path, hash string, extra url.Values) error {
	form := url.Values{"hashes": {hash}}
	for k, v := range extra {
		form[k] = v
	}
	_, err := c.do(ctx, call{method: http.MethodPost, path: path, form: form})
	return err
}

func (c *Client) Stop(ctx context.Context, hash string) error {
	return c.hashesCall(ctx, "torrents/stop", hash, nil)
}

func (c *Client) Start(ctx context.Context, hash string) error {
	return c.hashesCall(ctx, "torrents/start", hash, nil)
}

// Delete removes a torrent; with deleteFiles the engine removes exactly
// that torrent's own files.
func (c *Client) Delete(ctx context.Context, hash string, deleteFiles bool) error {
	return c.hashesCall(ctx, "torrents/delete", hash, url.Values{"deleteFiles": {strconv.FormatBool(deleteFiles)}})
}

func (c *Client) AddTags(ctx context.Context, hash string, tags ...string) error {
	return c.hashesCall(ctx, "torrents/addTags", hash, url.Values{"tags": {strings.Join(tags, ",")}})
}

func (c *Client) RemoveTags(ctx context.Context, hash string, tags ...string) error {
	return c.hashesCall(ctx, "torrents/removeTags", hash, url.Values{"tags": {strings.Join(tags, ",")}})
}

func (c *Client) SetLimits(ctx context.Context, hash string, download, upload int64) error {
	if err := c.hashesCall(ctx, "torrents/setDownloadLimit", hash, url.Values{"limit": {strconv.FormatInt(download, 10)}}); err != nil {
		return err
	}
	return c.hashesCall(ctx, "torrents/setUploadLimit", hash, url.Values{"limit": {strconv.FormatInt(upload, 10)}})
}

func (c *Client) Recheck(ctx context.Context, hash string) error {
	return c.hashesCall(ctx, "torrents/recheck", hash, nil)
}
