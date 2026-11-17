// Package fakeqb is an in-process stand-in for the qBittorrent 5.1 WebUI
// API subset the adapter uses. It reproduces the behaviours the service
// depends on: SID login, Referer/Origin CSRF checks, stop/start naming,
// "200 does not mean it happened", stopCondition=MetadataReceived and
// file priorities. Tests drive download progress with Tick.
package fakeqb

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openplayx/bittorrent-server/src/internal/bencode"
)

type File struct {
	Name     string
	Size     int64
	Progress float64
	Priority int
}

type Torrent struct {
	Hash, V1, V2   string
	Name           string
	SavePath       string
	Tags           []string
	Stopped        bool
	StopOnMetadata bool
	HasMetadata    bool
	Files          []File
	DlLimit        int64
	UpLimit        int64
	Error          bool
	AddedOn        int64
}

type Fake struct {
	mu       sync.Mutex
	Server   *httptest.Server
	Username string
	Password string
	Version  string
	torrents map[string]*Torrent
	order    []string
	metadata map[string][]File // v1/v2 hash -> files for magnets
	sids     map[string]bool

	// Fault injection.
	Down          bool // every request fails at the transport level
	DropAddReply  bool // add succeeds but the reply is a 500
	RejectAdd     bool // add answers "Fails."
	IgnoreStop    bool // stop answers 200 but does nothing
	Calls         map[string]int
	OnFileWritten func(t *Torrent, f File) // lets tests materialise payload
	OnDeleteFiles func(t *Torrent)         // deleteFiles=true
	LastAddURLs   string                   // last torrents/add "urls" field
	clock         int64
}

func New() *Fake {
	f := &Fake{
		Username: "admin", Password: "secret", Version: "v5.1.0",
		torrents: map[string]*Torrent{}, metadata: map[string][]File{}, sids: map[string]bool{},
		Calls: map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) Close() { f.Server.Close() }

// Set changes fault-injection fields under the fake's lock.
func (f *Fake) Set(change func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// AddURLs returns the "urls" field of the last add request.
func (f *Fake) AddURLs() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.LastAddURLs
}

// CallCount reports how often an API path was called.
func (f *Fake) CallCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls[path]
}

// RegisterMetadata makes magnets with this hash resolve to files.
func (f *Fake) RegisterMetadata(hash string, files []File) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metadata[strings.ToLower(hash)] = files
}

// ExpireSessions invalidates all SIDs (engine restart / timeout).
func (f *Fake) ExpireSessions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sids = map[string]bool{}
}

// Get returns a copy of a torrent by any hash.
func (f *Fake) Get(hash string) (Torrent, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.find(hash)
	if t == nil {
		return Torrent{}, false
	}
	c := *t
	c.Files = append([]File(nil), t.Files...)
	return c, true
}

// AddExternal inserts a torrent not created by the service.
func (f *Fake) AddExternal(t Torrent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Hash == "" {
		t.Hash = t.V1
	}
	f.clock++
	t.AddedOn = 1_700_000_000 + f.clock
	f.torrents[t.Hash] = &t
	f.order = append(f.order, t.Hash)
}

// RemoveOutside deletes a torrent as if a user did it in the WebUI.
func (f *Fake) RemoveOutside(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.find(hash); t != nil {
		delete(f.torrents, t.Hash)
	}
}

// Tick resolves pending metadata and advances running downloads by step.
func (f *Fake) Tick(step float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.order {
		t := f.torrents[h]
		if t == nil {
			continue
		}
		if !t.HasMetadata && !t.Stopped {
			if files, ok := f.metadata[t.V1]; ok || f.metadata[t.V2] != nil {
				if !ok {
					files = f.metadata[t.V2]
				}
				t.Files = append([]File(nil), files...)
				for i := range t.Files {
					t.Files[i].Priority = 1
				}
				t.HasMetadata = true
				if t.StopOnMetadata {
					t.Stopped = true
					t.StopOnMetadata = false
				}
			}
			continue
		}
		if t.Stopped || !t.HasMetadata {
			continue
		}
		for i := range t.Files {
			fl := &t.Files[i]
			if fl.Priority == 0 || fl.Progress >= 1 {
				continue
			}
			fl.Progress += step
			if fl.Progress >= 1 {
				fl.Progress = 1
				if f.OnFileWritten != nil {
					f.OnFileWritten(t, *fl)
				}
			}
		}
	}
}

func (f *Fake) find(hash string) *Torrent {
	hash = strings.ToLower(hash)
	if t, ok := f.torrents[hash]; ok {
		return t
	}
	for _, t := range f.torrents {
		if t.V1 == hash || t.V2 == hash {
			return t
		}
	}
	return nil
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down := f.Down
	f.mu.Unlock()
	if down {
		// Simulate an unreachable engine.
		hj, ok := w.(http.Hijacker)
		if ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	// CSRF: qBittorrent rejects mismatching Origin/Referer with 401.
	host := "http://" + r.Host
	if o := r.Header.Get("Origin"); o != "" && o != host {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if ref := r.Header.Get("Referer"); ref != "" && !strings.HasPrefix(ref, host) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v2/") {
		// Static WebUI assets; the real engine serves its login page here.
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<title>qBittorrent WebUI</title> host="+r.Host)
		return
	}
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "unexpected Authorization header", http.StatusBadRequest)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/")
	f.mu.Lock()
	f.Calls[path]++
	f.mu.Unlock()
	if path == "auth/login" {
		_ = r.ParseForm()
		if r.PostForm.Get("username") != f.Username || r.PostForm.Get("password") != f.Password {
			_, _ = io.WriteString(w, "Fails.")
			return
		}
		sid := strconv.FormatInt(time.Now().UnixNano(), 36)
		f.mu.Lock()
		f.sids[sid] = true
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: sid, Path: "/"})
		_, _ = io.WriteString(w, "Ok.")
		return
	}
	c, err := r.Cookie("SID")
	f.mu.Lock()
	ok := err == nil && f.sids[c.Value]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	switch path {
	case "app/version":
		_, _ = io.WriteString(w, f.Version)
	case "app/webapiVersion":
		_, _ = io.WriteString(w, "2.11.4")
	case "torrents/add":
		f.add(w, r)
	case "torrents/info":
		f.info(w, r)
	case "torrents/files":
		f.files(w, r)
	default:
		_ = r.ParseForm()
		f.control(w, path, r.Form)
	}
}

func (f *Fake) add(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RejectAdd {
		_, _ = io.WriteString(w, "Fails.")
		return
	}
	t := &Torrent{
		SavePath: r.FormValue("savepath"), Tags: strings.Split(r.FormValue("tags"), ","),
		Stopped: r.FormValue("stopped") == "true", StopOnMetadata: r.FormValue("stopCondition") == "MetadataReceived",
	}
	t.DlLimit, _ = strconv.ParseInt(r.FormValue("dlLimit"), 10, 64)
	t.UpLimit, _ = strconv.ParseInt(r.FormValue("upLimit"), 10, 64)
	f.LastAddURLs = r.FormValue("urls")
	if u := r.FormValue("urls"); u != "" {
		q, _ := url.ParseQuery(strings.TrimPrefix(u, "magnet:?"))
		for _, xt := range q["xt"] {
			if h, ok := strings.CutPrefix(xt, "urn:btih:"); ok {
				t.V1 = strings.ToLower(h)
			}
			if h, ok := strings.CutPrefix(xt, "urn:btmh:1220"); ok {
				t.V2 = strings.ToLower(h)
			}
		}
		t.Name = q.Get("dn")
		if t.Name == "" {
			t.Name = t.V1
		}
	} else if fh, _, err := r.FormFile("torrents"); err == nil {
		data, _ := io.ReadAll(fh)
		root, err := bencode.Decode(data)
		if err != nil {
			http.Error(w, "invalid", http.StatusUnsupportedMediaType)
			return
		}
		info := root.Dict["info"]
		if _, ok := info.Dict["pieces"]; ok {
			s := sha1.Sum(info.Raw)
			t.V1 = hex.EncodeToString(s[:])
		}
		if mv, ok := info.Dict["meta version"]; ok && mv.Int == 2 {
			s := sha256.Sum256(info.Raw)
			t.V2 = hex.EncodeToString(s[:])
		}
		t.Name = string(info.Dict["name"].Str)
		t.HasMetadata = true
		if files, ok := info.Dict["files"]; ok {
			for _, fl := range files.List {
				parts := []string{t.Name}
				for _, p := range fl.Dict["path"].List {
					parts = append(parts, string(p.Str))
				}
				t.Files = append(t.Files, File{Name: strings.Join(parts, "/"), Size: fl.Dict["length"].Int, Priority: 1})
			}
		} else {
			t.Files = []File{{Name: t.Name, Size: info.Dict["length"].Int, Priority: 1}}
		}
	} else {
		http.Error(w, "missing", http.StatusBadRequest)
		return
	}
	t.Hash = t.V1
	if t.Hash == "" {
		t.Hash = t.V2[:40]
	}
	if f.find(t.Hash) != nil || (t.V2 != "" && f.find(t.V2) != nil) {
		_, _ = io.WriteString(w, "Fails.")
		return
	}
	f.clock++
	t.AddedOn = 1_700_000_000 + f.clock
	f.torrents[t.Hash] = t
	f.order = append(f.order, t.Hash)
	if f.DropAddReply {
		http.Error(w, "lost", http.StatusInternalServerError)
		return
	}
	_, _ = io.WriteString(w, "Ok.")
}

func (t *Torrent) state() string {
	if t.Error {
		return "error"
	}
	if !t.HasMetadata {
		if t.Stopped {
			return "stoppedDL"
		}
		return "metaDL"
	}
	done := true
	for _, fl := range t.Files {
		if fl.Priority > 0 && fl.Progress < 1 {
			done = false
		}
	}
	switch {
	case t.Stopped && done:
		return "stoppedUP"
	case t.Stopped:
		return "stoppedDL"
	case done:
		return "stalledUP"
	default:
		return "downloading"
	}
}

func (f *Fake) info(w http.ResponseWriter, r *http.Request) {
	tag := r.URL.Query().Get("tag")
	only := r.URL.Query().Get("hashes")
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []map[string]any{}
	hashes := make([]string, 0, len(f.torrents))
	for h := range f.torrents {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	for _, h := range hashes {
		t := f.torrents[h]
		if tag != "" && !contains(t.Tags, tag) {
			continue
		}
		if only != "" && only != t.Hash {
			continue
		}
		var size, total, completed int64
		for _, fl := range t.Files {
			total += fl.Size
			if fl.Priority > 0 {
				size += fl.Size
				completed += int64(float64(fl.Size) * fl.Progress)
			}
		}
		progress := 0.0
		if size > 0 {
			progress = float64(completed) / float64(size)
		}
		out = append(out, map[string]any{
			"hash": t.Hash, "infohash_v1": t.V1, "infohash_v2": t.V2, "name": t.Name, "state": t.state(),
			"progress": progress, "size": size, "total_size": total, "completed": completed, "amount_left": size - completed,
			"downloaded": completed, "uploaded": 0, "dlspeed": 1000, "upspeed": 0, "dl_limit": t.DlLimit, "up_limit": t.UpLimit,
			"num_seeds": 1, "num_leechs": 0, "ratio": 0, "eta": 10, "save_path": t.SavePath, "tags": strings.Join(t.Tags, ", "),
			"has_metadata": t.HasMetadata, "added_on": t.AddedOn,
		})
	}
	_ = json.NewEncoder(w).Encode(out)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if strings.TrimSpace(x) == v {
			return true
		}
	}
	return false
}

func (f *Fake) files(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.find(r.URL.Query().Get("hash"))
	if t == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	out := []map[string]any{}
	for i, fl := range t.Files {
		out = append(out, map[string]any{"index": i, "name": fl.Name, "size": fl.Size, "progress": fl.Progress, "priority": fl.Priority, "availability": 1})
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *Fake) control(w http.ResponseWriter, path string, form url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch path {
	case "torrents/filePrio":
		t := f.find(form.Get("hash"))
		if t == nil {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		prio, _ := strconv.Atoi(form.Get("priority"))
		for _, id := range strings.Split(form.Get("id"), "|") {
			i, err := strconv.Atoi(id)
			if err != nil || i < 0 || i >= len(t.Files) {
				http.Error(w, "Conflict", http.StatusConflict)
				return
			}
			t.Files[i].Priority = prio
		}
		return
	}
	// Bulk endpoints answer 200 even for unknown hashes, like the engine.
	t := f.find(form.Get("hashes"))
	if t == nil {
		return
	}
	switch path {
	case "torrents/stop":
		if !f.IgnoreStop {
			t.Stopped = true
		}
	case "torrents/start":
		t.Stopped = false
	case "torrents/delete":
		if form.Get("deleteFiles") == "true" && f.OnDeleteFiles != nil {
			f.OnDeleteFiles(t)
		}
		delete(f.torrents, t.Hash)
	case "torrents/addTags":
		for _, tag := range strings.Split(form.Get("tags"), ",") {
			if tag = strings.TrimSpace(tag); tag != "" && !contains(t.Tags, tag) {
				t.Tags = append(t.Tags, tag)
			}
		}
	case "torrents/removeTags":
		drop := map[string]bool{}
		for _, tag := range strings.Split(form.Get("tags"), ",") {
			drop[strings.TrimSpace(tag)] = true
		}
		kept := t.Tags[:0]
		for _, tag := range t.Tags {
			if !drop[strings.TrimSpace(tag)] {
				kept = append(kept, tag)
			}
		}
		t.Tags = kept
	case "torrents/setDownloadLimit":
		t.DlLimit, _ = strconv.ParseInt(form.Get("limit"), 10, 64)
	case "torrents/setUploadLimit":
		t.UpLimit, _ = strconv.ParseInt(form.Get("limit"), 10, 64)
	case "torrents/recheck":
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}
