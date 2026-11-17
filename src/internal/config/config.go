// Package config loads config.json (non-secret settings) and secrets.json
// (API token digests and engine WebUI credentials, mode 0600).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Config struct {
	Listen      string `json:"listen"`
	TLSCertFile string `json:"tls_cert_file,omitempty"`
	TLSKeyFile  string `json:"tls_key_file,omitempty"`
	// DataDir is accepted for compatibility and unused: the service keeps
	// no state of its own.
	DataDir     string `json:"data_dir,omitempty"`
	SecretsFile string `json:"secrets_file"`
	// PublicName is shown in capabilities so the App can label the instance.
	PublicName   string        `json:"public_name"`
	Engine       Engine        `json:"engine"`
	DownloadRoot string        `json:"download_root"`
	Destinations []Destination `json:"destinations"`
	Limits       Limits        `json:"limits"`
	Seeding      Seeding       `json:"seeding"`
	Network      Network       `json:"network"`
	Files        Files         `json:"files"`
	WebUI        WebUI         `json:"webui"`
	// ReconcileIntervalSeconds controls how often engine state is projected.
	ReconcileIntervalSeconds int `json:"reconcile_interval_seconds"`
	// RetentionDays is accepted for compatibility and unused.
	RetentionDays int `json:"retention_days,omitempty"`
}

type Engine struct {
	URL string `json:"url"`
	// SupportedVersions pins the qBittorrent versions the adapter was
	// verified against, e.g. ["v5.1.0"]. Startup refuses others.
	SupportedVersions []string `json:"supported_versions"`
	MinWebAPIVersion  string   `json:"min_webapi_version"`
	TimeoutSeconds    int      `json:"timeout_seconds"`
}

// Destination maps a logical destination_ref to a directory below
// DownloadRoot. Callers never see or supply the absolute path.
type Destination struct {
	Ref     string `json:"ref"`
	Name    string `json:"name"`
	SubPath string `json:"sub_path"`
	// MaxTaskBytes overrides Limits.MaxTaskBytes when > 0.
	MaxTaskBytes int64 `json:"max_task_bytes,omitempty"`
}

type Limits struct {
	MaxActiveTasks      int   `json:"max_active_tasks"`
	MaxTaskBytes        int64 `json:"max_task_bytes"`
	MinFreeBytes        int64 `json:"min_free_bytes"`
	MaxTorrentFileBytes int64 `json:"max_torrent_file_bytes"`
	MaxMagnetLength     int   `json:"max_magnet_length"`
	MaxDownloadRate     int64 `json:"max_download_rate"` // bytes/s, 0 = unlimited
	MaxUploadRate       int64 `json:"max_upload_rate"`
	PlanTTLSeconds      int   `json:"plan_ttl_seconds"`
	InputTTLSeconds     int   `json:"input_ttl_seconds"`
	RequestsPerMinute   int   `json:"requests_per_minute"`
}

type Seeding struct {
	RatioLimit         float64 `json:"ratio_limit"`          // -1 = engine default, -2 = unlimited
	SeedingTimeMinutes int     `json:"seeding_time_minutes"` // -1 / -2 like above
}

type Network struct {
	// AllowPrivateNetworks permits tracker/web-seed/peer hosts in private,
	// loopback or link-local ranges. Only isolated test deployments set it.
	AllowPrivateNetworks bool     `json:"allow_private_networks"`
	TrackerSchemes       []string `json:"tracker_schemes"`
}

// WebUI exposes the engine's native WebUI through this server: every path
// outside /api/ (plus the WebUI's own /api/v2/) is proxied to the engine,
// with dangerous endpoints blocked and API-managed torrents protected.
type WebUI struct {
	Enabled bool `json:"enabled"`
	// LoginFailuresPerClient within 15 minutes before a client IP is refused.
	LoginFailuresPerClient int `json:"login_failures_per_client"`
}

type Files struct {
	ContentAccess bool `json:"content_access"`
}

type Secrets struct {
	APITokens []APIToken `json:"api_tokens"`
	Engine    struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"engine"`
}

// APIToken stores only the SHA-256 of a bearer token. Caller is the stable
// owner identity attached to every task the token creates.
type APIToken struct {
	Caller      string `json:"caller"`
	TokenSHA256 string `json:"token_sha256"`
}

var refPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func Default() Config {
	return Config{
		Listen:      "127.0.0.1:8780",
		DataDir:     "data",
		SecretsFile: "secrets.json",
		PublicName:  "OpenPlayX BitTorrent",
		Engine: Engine{
			URL:               "http://127.0.0.1:8080",
			SupportedVersions: []string{"v5.1.0"},
			MinWebAPIVersion:  "2.11.0",
			TimeoutSeconds:    10,
		},
		DownloadRoot: "/downloads",
		Destinations: []Destination{{Ref: "default", Name: "Downloads", SubPath: "default"}},
		Limits: Limits{
			MaxActiveTasks:      20,
			MaxTaskBytes:        64 << 30,
			MinFreeBytes:        2 << 30,
			MaxTorrentFileBytes: 4 << 20,
			MaxMagnetLength:     4096,
			PlanTTLSeconds:      1800,
			InputTTLSeconds:     3600,
			RequestsPerMinute:   240,
		},
		Seeding:                  Seeding{RatioLimit: 1, SeedingTimeMinutes: 1440},
		WebUI:                    WebUI{Enabled: false, LoginFailuresPerClient: 5},
		Network:                  Network{TrackerSchemes: []string{"http", "https", "udp"}},
		ReconcileIntervalSeconds: 5,
		RetentionDays:            30,
	}
}

// Load reads config.json over the defaults; relative paths resolve against
// the config file's directory.
func Load(path string) (Config, error) {
	c := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	base := filepath.Dir(path)
	for _, p := range []*string{&c.DataDir, &c.SecretsFile, &c.TLSCertFile, &c.TLSKeyFile} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.Listen == "" || c.Engine.URL == "" {
		return errors.New("config: listen and engine.url are required")
	}
	if !filepath.IsAbs(c.DownloadRoot) {
		return errors.New("config: download_root must be absolute")
	}
	if len(c.Engine.SupportedVersions) == 0 {
		return errors.New("config: engine.supported_versions must pin at least one version")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("config: tls_cert_file and tls_key_file go together")
	}
	if len(c.Destinations) == 0 {
		return errors.New("config: at least one destination is required")
	}
	seen := map[string]bool{}
	for _, d := range c.Destinations {
		if !refPattern.MatchString(d.Ref) || seen[d.Ref] {
			return fmt.Errorf("config: invalid or duplicate destination ref %q", d.Ref)
		}
		seen[d.Ref] = true
		clean := filepath.Clean(d.SubPath)
		if d.SubPath == "" || filepath.IsAbs(d.SubPath) || clean == "." || clean == ".." ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("config: destination %q sub_path must stay below download_root", d.Ref)
		}
	}
	l := c.Limits
	if l.MaxActiveTasks <= 0 || l.MaxTaskBytes <= 0 || l.MaxTorrentFileBytes <= 0 ||
		l.PlanTTLSeconds <= 0 || l.InputTTLSeconds <= 0 || l.RequestsPerMinute <= 0 ||
		l.MaxMagnetLength <= 0 || l.MinFreeBytes < 0 || l.MaxDownloadRate < 0 || l.MaxUploadRate < 0 {
		return errors.New("config: limits must be positive")
	}
	if c.ReconcileIntervalSeconds <= 0 || c.Engine.TimeoutSeconds <= 0 {
		return errors.New("config: intervals must be positive")
	}
	return nil
}

func (c *Config) DestinationPath(d Destination) string {
	return filepath.Join(c.DownloadRoot, filepath.Clean(d.SubPath))
}

func (c *Config) Destination(ref string) (Destination, bool) {
	for _, d := range c.Destinations {
		if d.Ref == ref {
			return d, true
		}
	}
	return Destination{}, false
}

// Fingerprint changes whenever a destination mapping or quota changes, so
// plans made under an earlier policy become invalid.
func (d Destination) Fingerprint(c *Config) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%d|%d", d.Ref, c.DestinationPath(d), d.MaxTaskBytes, c.Limits.MaxTaskBytes))
	return hex.EncodeToString(h[:8])
}

func LoadSecrets(path string) (Secrets, error) {
	var s Secrets
	info, err := os.Stat(path)
	if err != nil {
		return s, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return s, fmt.Errorf("secrets: %s must not be group/world accessible", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("secrets: %w", err)
	}
	if len(s.APITokens) == 0 {
		return s, errors.New("secrets: at least one api token is required")
	}
	for _, t := range s.APITokens {
		if t.Caller == "" || len(t.TokenSHA256) != 64 {
			return s, errors.New("secrets: api token needs caller and 64-hex token_sha256")
		}
	}
	return s, nil
}

func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
