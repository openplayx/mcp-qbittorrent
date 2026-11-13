package torrents

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"

	"github.com/openplayx/bittorrent-server/internal/bencode"
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
	cases := map[string]string{
		"http://example.com/x.torrent": "invalid_magnet",
		"magnet:?dn=x":                 "invalid_magnet",
		"magnet:?xt=urn:btih:zz":       "invalid_magnet",
		"magnet:?xt=urn:btih:" + hash + "&xs=http://example.com/a.torrent":    "unsupported_magnet_parameter",
		"magnet:?xt=urn:btih:" + hash + "&as=http://example.com/a.torrent":    "unsupported_magnet_parameter",
		"magnet:?xt=urn:btih:" + hash + "&tr=http://127.0.0.1/announce":       "network_address_not_allowed",
		"magnet:?xt=urn:btih:" + hash + "&tr=http://192.168.1.2/announce":     "network_address_not_allowed",
		"magnet:?xt=urn:btih:" + hash + "&tr=http://[::1]/announce":           "network_address_not_allowed",
		"magnet:?xt=urn:btih:" + hash + "&tr=http://localhost/announce":       "network_address_not_allowed",
		"magnet:?xt=urn:btih:" + hash + "&tr=http://rebind.example.com/a":     "network_address_not_allowed",
		"magnet:?xt=urn:btih:" + hash + "&tr=file:///etc/passwd":              "tracker_not_allowed",
		"magnet:?xt=urn:btih:" + hash + "&tr=http://u:p@example.com/announce": "tracker_not_allowed",
		"magnet:?xt=urn:btih:" + hash + "&ws=ftp://example.com/file":          "tracker_not_allowed",
		"magnet:?xt=urn:btih:" + hash + "&x.pe=10.0.0.1:6881":                 "network_address_not_allowed",
	}
	for raw, code := range cases {
		_, err := ParseMagnet(ctx, raw, 4096, strict)
		ie, ok := err.(*InputError)
		if !ok || ie.Code != code {
			t.Errorf("%s: got %v want %s", raw, err, code)
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
		"tracker":     makeTorrent("Show", nil, "http://10.1.1.1/announce"),
		"not bencode": []byte("garbage"),
	}
	for name, data := range bad {
		if _, err := ParseTorrent(ctx, data, strict); err == nil {
			t.Errorf("%s accepted", name)
		}
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
