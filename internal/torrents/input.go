package torrents

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/openplayx/bittorrent-server/internal/bencode"
)

// Identity is the BitTorrent identity of a payload. A hybrid torrent has
// both; the engine is matched on whichever it reports.
type Identity struct {
	V1 string `json:"v1,omitempty"` // 40 hex, SHA-1
	V2 string `json:"v2,omitempty"` // 64 hex, SHA-256
}

func (i Identity) Version() string {
	switch {
	case i.V1 != "" && i.V2 != "":
		return "hybrid"
	case i.V2 != "":
		return "v2"
	default:
		return "v1"
	}
}

// Matches reports whether engine-reported hashes describe the same payload.
func (i Identity) Matches(v1, v2 string) bool {
	return (i.V1 != "" && strings.EqualFold(i.V1, v1)) || (i.V2 != "" && strings.EqualFold(i.V2, v2))
}

type PreviewFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Input is a validated, sanitised torrent source.
type Input struct {
	Kind     string        // "magnet" or "torrent_file"
	Identity Identity      //
	Name     string        // display name (may be empty for magnets)
	Magnet   string        // sanitised magnet; never returned to clients
	Torrent  []byte        // raw .torrent; never returned to clients
	Files    []PreviewFile // nil when metadata is unknown
	Size     int64         // 0 when unknown
	Private  bool
	Trackers int
}

// InputError carries a stable error code for the API layer.
type InputError struct{ Code string }

func (e *InputError) Error() string { return e.Code }

func bad(code string) error { return &InputError{Code: code} }

// Policy restricts every nested network address in an input (trackers, web
// seeds, peer hints), not just the outer scheme.
type Policy struct {
	AllowPrivate   bool
	TrackerSchemes []string
	Resolver       func(ctx context.Context, host string) ([]netip.Addr, error)
}

func (p Policy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if p.Resolver != nil {
		return p.Resolver(ctx, host)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func privateAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(a) // CGNAT
}

func (p Policy) checkHost(ctx context.Context, host string) error {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return bad("invalid_network_address")
	}
	if p.AllowPrivate {
		return nil
	}
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if privateAddr(a) {
			return bad("network_address_not_allowed")
		}
		return nil
	}
	if !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") {
		return bad("network_address_not_allowed")
	}
	addrs, err := p.resolve(ctx, host)
	if err != nil {
		return nil // unresolvable now: the engine will fail the same way
	}
	for _, a := range addrs {
		if privateAddr(a) {
			return bad("network_address_not_allowed")
		}
	}
	return nil
}

func (p Policy) checkURL(ctx context.Context, raw string, schemes []string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || !slices.Contains(schemes, strings.ToLower(u.Scheme)) {
		return bad("tracker_not_allowed")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
			return bad("tracker_not_allowed")
		}
	}
	return p.checkHost(ctx, u.Hostname())
}

const maxTrackers = 64

// ParseMagnet validates a magnet link and rebuilds it from the allowed
// parameters only. Parameters that make the engine fetch arbitrary URLs
// (xs, as, kt, mt) are rejected instead of silently dropped.
func ParseMagnet(ctx context.Context, raw string, maxLen int, p Policy) (*Input, error) {
	if len(raw) > maxLen {
		return nil, bad("magnet_too_long")
	}
	if !strings.HasPrefix(strings.ToLower(raw), "magnet:?") {
		return nil, bad("invalid_magnet")
	}
	q, err := url.ParseQuery(raw[len("magnet:?"):])
	if err != nil {
		return nil, bad("invalid_magnet")
	}
	in := &Input{Kind: "magnet"}
	out := url.Values{}
	for key, values := range q {
		switch key {
		case "xt":
			for _, v := range values {
				lv := strings.ToLower(v)
				switch {
				case strings.HasPrefix(lv, "urn:btih:"):
					h := v[len("urn:btih:"):]
					switch len(h) {
					case 40:
						if _, err := hex.DecodeString(h); err != nil {
							return nil, bad("invalid_magnet")
						}
						in.Identity.V1 = strings.ToLower(h)
					case 32:
						b, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
						if err != nil {
							return nil, bad("invalid_magnet")
						}
						in.Identity.V1 = hex.EncodeToString(b)
					default:
						return nil, bad("invalid_magnet")
					}
				case strings.HasPrefix(lv, "urn:btmh:1220"):
					h := lv[len("urn:btmh:1220"):]
					if len(h) != 64 {
						return nil, bad("invalid_magnet")
					}
					if _, err := hex.DecodeString(h); err != nil {
						return nil, bad("invalid_magnet")
					}
					in.Identity.V2 = h
				default:
					return nil, bad("invalid_magnet")
				}
			}
		case "dn":
			in.Name = safeDisplayName(values[0])
		case "xl":
			if n, err := strconv.ParseInt(values[0], 10, 64); err == nil && n > 0 {
				in.Size = n
			}
		case "tr":
			if len(values) > maxTrackers {
				return nil, bad("too_many_trackers")
			}
			for _, v := range values {
				if err := p.checkURL(ctx, v, p.TrackerSchemes); err != nil {
					return nil, err
				}
				out.Add("tr", v)
			}
			in.Trackers = len(values)
		case "ws":
			for _, v := range values {
				if err := p.checkURL(ctx, v, []string{"http", "https"}); err != nil {
					return nil, err
				}
				out.Add("ws", v)
			}
		case "x.pe":
			for _, v := range values {
				host, port, err := net.SplitHostPort(v)
				if n, perr := strconv.Atoi(port); err != nil || perr != nil || n <= 0 || n > 65535 {
					return nil, bad("invalid_magnet")
				}
				if err := p.checkHost(ctx, host); err != nil {
					return nil, err
				}
				out.Add("x.pe", v)
			}
		default:
			return nil, bad("unsupported_magnet_parameter")
		}
	}
	if in.Identity.V1 == "" && in.Identity.V2 == "" {
		return nil, bad("invalid_magnet")
	}
	// Rebuild deterministically so plans over the same input compare equal.
	var b strings.Builder
	b.WriteString("magnet:?")
	parts := []string{}
	if in.Identity.V1 != "" {
		parts = append(parts, "xt=urn:btih:"+in.Identity.V1)
	}
	if in.Identity.V2 != "" {
		parts = append(parts, "xt=urn:btmh:1220"+in.Identity.V2)
	}
	if in.Name != "" {
		parts = append(parts, "dn="+url.QueryEscape(in.Name))
	}
	for _, k := range []string{"tr", "ws", "x.pe"} {
		vs := out[k]
		slices.Sort(vs)
		for _, v := range vs {
			parts = append(parts, k+"="+url.QueryEscape(v))
		}
	}
	b.WriteString(strings.Join(parts, "&"))
	in.Magnet = b.String()
	return in, nil
}

func safeDisplayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len([]rune(s)) > 200 {
		s = string([]rune(s)[:200])
	}
	return strings.TrimSpace(s)
}

const maxFiles = 20000

// validComponent rejects anything that could escape the task directory on
// any platform: separators, traversal, NUL, drive letters, empty names.
func validComponent(c string) bool {
	if c == "" || c == "." || c == ".." || len(c) > 255 {
		return false
	}
	if strings.ContainsAny(c, "/\\\x00") || (len(c) >= 2 && c[1] == ':') {
		return false
	}
	return true
}

// ParseTorrent validates a .torrent and computes its v1/v2 identities from
// the exact info bytes.
func ParseTorrent(ctx context.Context, data []byte, p Policy) (*Input, error) {
	root, err := bencode.Decode(data)
	if err != nil || root.Kind != 'd' {
		return nil, bad("invalid_torrent")
	}
	info, ok := root.Dict["info"]
	if !ok || info.Kind != 'd' {
		return nil, bad("invalid_torrent")
	}
	in := &Input{Kind: "torrent_file", Torrent: data}
	name, ok := info.Dict["name"]
	if !ok || name.Kind != 's' || !validComponent(string(name.Str)) {
		return nil, bad("unsafe_torrent_path")
	}
	in.Name = safeDisplayName(string(name.Str))
	if pr, ok := info.Dict["private"]; ok && pr.Kind == 'i' && pr.Int == 1 {
		in.Private = true
	}
	mv, hasMV := info.Dict["meta version"]
	isV2 := hasMV && mv.Kind == 'i' && mv.Int == 2
	_, hasPieces := info.Dict["pieces"]
	if hasMV && !isV2 {
		return nil, bad("unsupported_torrent_version")
	}
	if hasPieces {
		s := sha1.Sum(info.Raw)
		in.Identity.V1 = hex.EncodeToString(s[:])
	}
	if isV2 {
		s := sha256.Sum256(info.Raw)
		in.Identity.V2 = hex.EncodeToString(s[:])
	}
	if in.Identity.V1 == "" && in.Identity.V2 == "" {
		return nil, bad("invalid_torrent")
	}

	add := func(path []string, size int64) error {
		if size < 0 || len(in.Files) >= maxFiles {
			return bad("invalid_torrent")
		}
		for _, c := range path {
			if !validComponent(c) {
				return bad("unsafe_torrent_path")
			}
		}
		in.Files = append(in.Files, PreviewFile{Path: strings.Join(path, "/"), Size: size})
		in.Size += size
		return nil
	}
	if isV2 {
		tree, ok := info.Dict["file tree"]
		if !ok || tree.Kind != 'd' {
			return nil, bad("invalid_torrent")
		}
		var walk func(v bencode.Value, path []string, depth int) error
		walk = func(v bencode.Value, path []string, depth int) error {
			if depth > 32 || v.Kind != 'd' {
				return bad("invalid_torrent")
			}
			if leaf, ok := v.Dict[""]; ok {
				l, ok := leaf.Dict["length"]
				if leaf.Kind != 'd' || !ok || l.Kind != 'i' || len(v.Dict) != 1 {
					return bad("invalid_torrent")
				}
				return add(path, l.Int)
			}
			keys := make([]string, 0, len(v.Dict))
			for k := range v.Dict {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				if err := walk(v.Dict[k], append(slices.Clone(path), k), depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		if err := walk(tree, nil, 0); err != nil {
			return nil, err
		}
		if len(in.Files) == 1 && in.Files[0].Path == in.Name {
			// single-file v2 torrent: file tree is {name: {"": ...}}
		} else {
			for i := range in.Files {
				in.Files[i].Path = in.Name + "/" + in.Files[i].Path
			}
		}
	} else if files, ok := info.Dict["files"]; ok {
		if files.Kind != 'l' {
			return nil, bad("invalid_torrent")
		}
		for _, f := range files.List {
			l, lok := f.Dict["length"]
			pth, pok := f.Dict["path"]
			if f.Kind != 'd' || !lok || !pok || l.Kind != 'i' || pth.Kind != 'l' || len(pth.List) == 0 {
				return nil, bad("invalid_torrent")
			}
			if attr, ok := f.Dict["attr"]; ok && attr.Kind == 's' && strings.Contains(string(attr.Str), "p") {
				continue // BEP 47 padding file: not a user-visible file
			}
			comps := []string{in.Name}
			for _, c := range pth.List {
				if c.Kind != 's' {
					return nil, bad("invalid_torrent")
				}
				comps = append(comps, string(c.Str))
			}
			if err := add(comps, l.Int); err != nil {
				return nil, err
			}
		}
	} else if l, ok := info.Dict["length"]; ok && l.Kind == 'i' {
		if err := add([]string{in.Name}, l.Int); err != nil {
			return nil, err
		}
	} else {
		return nil, bad("invalid_torrent")
	}
	if len(in.Files) == 0 {
		return nil, bad("invalid_torrent")
	}

	trackers := []string{}
	if a, ok := root.Dict["announce"]; ok && a.Kind == 's' {
		trackers = append(trackers, string(a.Str))
	}
	if al, ok := root.Dict["announce-list"]; ok && al.Kind == 'l' {
		for _, tier := range al.List {
			for _, t := range tier.List {
				if t.Kind == 's' {
					trackers = append(trackers, string(t.Str))
				}
			}
		}
	}
	if len(trackers) > maxTrackers {
		return nil, bad("too_many_trackers")
	}
	for _, t := range trackers {
		if err := p.checkURL(ctx, t, p.TrackerSchemes); err != nil {
			return nil, err
		}
	}
	if ul, ok := root.Dict["url-list"]; ok {
		seeds := []string{}
		if ul.Kind == 's' {
			seeds = append(seeds, string(ul.Str))
		}
		for _, s := range ul.List {
			if s.Kind == 's' {
				seeds = append(seeds, string(s.Str))
			}
		}
		for _, s := range seeds {
			if err := p.checkURL(ctx, s, []string{"http", "https"}); err != nil {
				return nil, err
			}
		}
	}
	in.Trackers = len(trackers)
	return in, nil
}

var videoExt = []string{".mkv", ".mp4", ".m4v", ".avi", ".mov", ".wmv", ".ts", ".m2ts", ".webm", ".flv", ".mpg", ".mpeg", ".rmvb"}

// IsVideo backs the "video" selection preset. Sample clips are excluded.
func IsVideo(path string) bool {
	lp := strings.ToLower(path)
	if strings.Contains(lp, "sample") {
		return false
	}
	for _, e := range videoExt {
		if strings.HasSuffix(lp, e) {
			return true
		}
	}
	return false
}
