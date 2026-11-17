package torrents

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"

	"github.com/openplayx/bittorrent-server/src/internal/bencode"
)

var strict = Policy{TrackerSchemes: []string{"http", "https", "udp"}, Resolver: func(_ context.Context, host string) ([]netip.Addr, error) {
	if host == "rebind.example.com" {
		return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}}

const hash = "0123456789abcdef0123456789abcdef01234567"

func TestParseMagnet(t *testing.T) {
	ctx := context.Background()
	in, err := ParseMagnet(ctx, "magnet:?xt=urn:btih:"+strings.ToUpper(hash)+"&dn=Test+Name&tr=udp://tracker.example.com:6969/announce", 4096, strict)
	if err != nil {
		t.Fatal(err)
	}
	if in.Identity.V1 != hash || in.Name != "Test Name" || in.Identity.Version() != "v1" {
		t.Fatalf("unexpected %+v", in)
	}
	if !strings.HasPrefix(in.Magnet, "magnet:?xt=urn:btih:"+hash) {
		t.Fatalf("magnet not rebuilt: %s", in.Magnet)
	}
	// base32 btih
	if in, err := ParseMagnet(ctx, "magnet:?xt=urn:btih:AERUKZ4JVPG66AJDIVTYTK6N54ASGRLH", 4096, strict); err != nil || in.Identity.V1 != hash {
		t.Fatalf("base32: %v %+v", err, in)
	}
	// v2 / hybrid
	v2 := strings.Repeat("ab", 32)
	in, err = ParseMagnet(ctx, "magnet:?xt=urn:btih:"+hash+"&xt=urn:btmh:1220"+v2, 4096, strict)
	if err != nil || in.Identity.Version() != "hybrid" {
		t.Fatalf("hybrid: %v", err)
	}
	rejected := map[string]string{
		"http://example.com/x.torrent": "invalid_magnet",
		"magnet:?dn=x":                 "invalid_magnet",
		"magnet:?xt=urn:btih:zz":       "invalid_magnet",
		"magnet:?xt=urn:btih:" + hash + "&xs=http://example.com/a.torrent": "unsupported_magnet_parameter",
		"magnet:?xt=urn:btih:" + hash + "&as=http://example.com/a.torrent": "unsupported_magnet_parameter",
	}
	for raw, code := range rejected {
		_, err := ParseMagnet(ctx, raw, 4096, strict)
		ie, ok := err.(*InputError)
		if !ok || ie.Code != code {
			t.Errorf("%s: got %v want %s", raw, err, code)
		}
	}
	// Disallowed nested addresses are dropped, the magnet is kept.
	dropped := map[string][2]int{ // raw suffix → {private, unsupported}
		"&tr=http://127.0.0.1/announce":       {1, 0},
		"&tr=http://192.168.1.2/announce":     {1, 0},
		"&tr=http://[::1]/announce":           {1, 0},
		"&tr=http://localhost/announce":       {1, 0},
		"&tr=http://rebind.example.com/a":     {1, 0}, // DNS answer is 10.0.0.5
		"&tr=file:///etc/passwd":              {0, 1},
		"&tr=http://u:p@example.com/announce": {0, 1},
		"&ws=ftp://example.com/file":          {0, 1},
		"&x.pe=10.0.0.1:6881":                 {1, 0},
		"&x.pe=bad":                           {0, 1},
	}
	for suffix, want := range dropped {
		raw := "magnet:?xt=urn:btih:" + hash + "&tr=udp://tracker.example.com:6969/announce" + suffix
		in, err := ParseMagnet(ctx, raw, 4096, strict)
		if err != nil {
			t.Errorf("%s: rejected: %v", suffix, err)
			continue
		}
		if in.Removed.PrivateAddress != want[0] || in.Removed.Unsupported != want[1] || in.Trackers != 1 {
			t.Errorf("%s: removed %+v trackers %d", suffix, in.Removed, in.Trackers)
		}
		if strings.Contains(in.Magnet, "127.0.0.1") || strings.Contains(in.Magnet, "rebind") || strings.Contains(in.Magnet, "10.0.0.1") ||
			strings.Contains(in.Magnet, "localhost") || !strings.Contains(in.Magnet, "tracker.example.com") {
			t.Errorf("%s: sanitised magnet %s", suffix, in.Magnet)
		}
	}
	if _, err := ParseMagnet(ctx, "magnet:?xt=urn:btih:"+hash+strings.Repeat("&dn=x", 1000), 100, strict); err == nil {
		t.Error("length limit not enforced")
	}
	open := strict
	open.AllowPrivate = true
	if _, err := ParseMagnet(ctx, "magnet:?xt=urn:btih:"+hash+"&tr=http://192.168.1.2:9000/announce", 4096, open); err != nil {
		t.Errorf("private allowed: %v", err)
	}
}

func makeTorrent(name string, files []any, announce string) []byte {
	info := map[string]any{"name": name, "piece length": 16384, "pieces": strings.Repeat("x", 20)}
	if files == nil {
		info["length"] = 100
	} else {
		info["files"] = files
	}
	root := map[string]any{"info": info}
	if announce != "" {
		root["announce"] = announce
	}
	return bencode.Encode(root)
}

func file(size int, path ...string) map[string]any {
	p := []any{}
	for _, c := range path {
		p = append(p, c)
	}
	return map[string]any{"length": size, "path": p}
}

func TestParseTorrent(t *testing.T) {
	ctx := context.Background()
	data := makeTorrent("Show", []any{file(10, "a.mkv"), file(5, "sub", "b.srt"), map[string]any{"length": 3, "path": []any{".pad", "3"}, "attr": "p"}}, "https://tracker.example.com/announce")
	in, err := ParseTorrent(ctx, data, strict)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := bencode.Decode(data)
	sum := sha1.Sum(root.Dict["info"].Raw)
	if in.Identity.V1 != hex.EncodeToString(sum[:]) {
		t.Fatal("v1 identity mismatch")
	}
	if len(in.Files) != 2 || in.Files[1].Path != "Show/sub/b.srt" || in.Size != 15 {
		t.Fatalf("files %+v", in.Files)
	}
	bad := map[string][]byte{
		"traversal":   makeTorrent("Show", []any{file(1, "..", "etc")}, ""),
		"slash":       makeTorrent("Show", []any{file(1, "a/b")}, ""),
		"backslash":   makeTorrent("Show", []any{file(1, `..\x`)}, ""),
		"drive":       makeTorrent("C:evil", nil, ""),
		"dotname":     makeTorrent("..", nil, ""),
		"not bencode": []byte("garbage"),
	}
	for name, data := range bad {
		if _, err := ParseTorrent(ctx, data, strict); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestTorrentTrackerFiltering(t *testing.T) {
	ctx := context.Background()
	info := map[string]any{"name": "Show", "piece length": 16384, "pieces": strings.Repeat("x", 20), "length": 100}
	data := bencode.Encode(map[string]any{
		"info":     info,
		"announce": "udp://tracker.leechers-paradise.example:6969/announce", // resolves fine in this test
		"announce-list": []any{
			[]any{"http://127.0.0.1:6969/announce"},
			[]any{"udp://tracker.example.com:6969/announce", "http://192.168.1.9/announce"},
		},
		"url-list": []any{"https://mirror.example.com/show", "http://10.0.0.8/show"},
		"comment":  "kept",
	})
	in, err := ParseTorrent(ctx, data, strict)
	if err != nil {
		t.Fatal(err)
	}
	if in.Removed.PrivateAddress != 3 || in.Trackers != 1 {
		t.Fatalf("removed %+v trackers %d", in.Removed, in.Trackers)
	}
	// The rewritten torrent keeps the same identity and drops the addresses.
	again, err := ParseTorrent(ctx, in.Torrent, strict)
	if err != nil {
		t.Fatal(err)
	}
	if again.Identity != in.Identity || again.Removed.Total() != 0 {
		t.Fatalf("rewritten torrent: %+v %+v", again.Identity, again.Removed)
	}
	for _, leak := range []string{"127.0.0.1", "192.168.1.9", "10.0.0.8"} {
		if strings.Contains(string(in.Torrent), leak) {
			t.Fatalf("%s still in torrent", leak)
		}
	}
	if !strings.Contains(string(in.Torrent), "mirror.example.com") || !strings.Contains(string(in.Torrent), "4:kept") {
		t.Fatal("allowed fields lost")
	}
	// A torrent without disallowed addresses is passed through byte for byte.
	clean := makeTorrent("Clean", nil, "https://tracker.example.com/announce")
	in, _ = ParseTorrent(ctx, clean, strict)
	if string(in.Torrent) != string(clean) {
		t.Fatal("clean torrent modified")
	}
}

func TestV2Torrent(t *testing.T) {
	tree := map[string]any{"a.mkv": map[string]any{"": map[string]any{"length": 7, "pieces root": strings.Repeat("r", 32)}}}
	info := map[string]any{"name": "Pack", "piece length": 16384, "meta version": 2, "file tree": tree}
	in, err := ParseTorrent(context.Background(), bencode.Encode(map[string]any{"info": info}), strict)
	if err != nil {
		t.Fatal(err)
	}
	if in.Identity.Version() != "v2" || len(in.Identity.V2) != 64 || in.Files[0].Path != "Pack/a.mkv" {
		t.Fatalf("%+v", in)
	}
	info["pieces"] = strings.Repeat("x", 20)
	info["files"] = []any{file(7, "a.mkv")}
	in, err = ParseTorrent(context.Background(), bencode.Encode(map[string]any{"info": info}), strict)
	if err != nil || in.Identity.Version() != "hybrid" {
		t.Fatalf("hybrid %v %+v", err, in)
	}
}

func TestStateMapping(t *testing.T) {
	cases := map[string]string{
		"metaDL": StateMetadataPending, "stoppedDL": StatePaused, "downloading": StateDownloading,
		"stalledDL": StateStalled, "stalledUP": StateSeeding, "stoppedUP": StateCompleted,
		"checkingResumeData": StateChecking, "missingFiles": StateError, "queuedDL": StateQueued,
	}
	for in, want := range cases {
		if got := mapEngineState(in, true); got != want {
			t.Errorf("%s: %s want %s", in, got, want)
		}
	}
	if mapEngineState("stoppedDL", false) != StateMetadataPending {
		t.Error("stopped without metadata should be metadata_pending")
	}
}
