package torrents

import (
	"context"
	"time"

	"github.com/openplayx/bittorrent-server/internal/qbittorrent"
)

// Run executes the engine-side automation periodically until ctx ends:
// initial selection for torrents tagged pending, and the disk watermark.
// It keeps no state of its own; every decision is read from engine tags.
func (s *Service) Run(ctx context.Context) {
	interval := time.Duration(s.cfg.ReconcileIntervalSeconds) * time.Second
	for {
		s.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (s *Service) Reconcile(ctx context.Context) {
	list, err := s.engine.Info(ctx, "")
	if err != nil {
		if s.healthy.Swap(false) {
			s.log.Warn("engine unavailable", "err", err)
		}
		return
	}
	if !s.healthy.Swap(true) {
		s.log.Info("engine available")
	}
	s.lastSeen.Store(time.Now().Unix())
	free := s.free()
	diskLow := free >= 0 && free < s.cfg.Limits.MinFreeBytes
	diskOK := free < 0 || free >= s.cfg.Limits.MinFreeBytes+s.cfg.Limits.MinFreeBytes/10
	for _, t := range list {
		s.reconcileOne(ctx, t, free, diskLow, diskOK)
	}
}

func (s *Service) reconcileOne(ctx context.Context, t qbittorrent.Torrent, free int64, diskLow, diskOK bool) {
	defer s.lock(t.Hash)()
	tags := tagSet(t)
	if tags[TagPending] && t.HasMetadata {
		s.applyInitialSelection(ctx, t, free)
		return
	}
	// Continuous disk watermark for every torrent in the engine.
	downloading := !isStopped(t.State) && t.Progress < 1
	switch {
	case diskLow && downloading:
		if err := s.engine.Stop(ctx, t.Hash); err == nil {
			_ = s.engine.AddTags(ctx, t.Hash, TagHoldDisk)
			s.log.Warn("paused for low disk", "task", t.Hash)
		}
	case diskOK && tags[TagHoldDisk]:
		if err := s.engine.RemoveTags(ctx, t.Hash, TagHoldDisk); err == nil {
			_ = s.engine.Start(ctx, t.Hash)
		}
	}
}

// Health is served by /api/readyz and capabilities; the engine is probed live.
type Health struct {
	Engine        bool   `json:"engine"`
	EngineVersion string `json:"engine_version,omitempty"`
	WebAPIVersion string `json:"webapi_version,omitempty"`
}

func (s *Service) Health(ctx context.Context) Health {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	h := Health{Engine: s.engine.Ping(ctx) == nil}
	h.EngineVersion, h.WebAPIVersion = s.engine.Versions()
	return h
}

// ---- capabilities & destinations ----

type DestinationView struct {
	Ref          string `json:"destination_ref"`
	Name         string `json:"name"`
	FreeBytes    *int64 `json:"free_bytes,omitempty"`
	MaxTaskBytes int64  `json:"max_task_bytes"`
}

func (s *Service) destinationView(ref string) DestinationView {
	d, _ := s.cfg.Destination(ref)
	path := s.cfg.DestinationPath(d)
	v := DestinationView{Ref: d.Ref, Name: d.Name, MaxTaskBytes: s.maxTaskBytes(path)}
	if s.freeBytes != nil {
		_ = mkdirAll(path)
		if free, err := s.freeBytes(path); err == nil {
			v.FreeBytes = &free
		}
	}
	return v
}

func (s *Service) Destinations() []DestinationView {
	out := []DestinationView{}
	for _, d := range s.cfg.Destinations {
		out = append(out, s.destinationView(d.Ref))
	}
	return out
}

func (s *Service) Capabilities(ctx context.Context, serviceVersion string) map[string]any {
	h := s.Health(ctx)
	l := s.cfg.Limits
	return map[string]any{
		"service":           map[string]any{"name": s.cfg.PublicName, "version": serviceVersion, "api_version": "v1"},
		"engine":            map[string]any{"name": "qBittorrent", "version": h.EngineVersion, "webapi_version": h.WebAPIVersion, "available": h.Engine},
		"inputs":            []string{"magnet", "torrent_file"},
		"torrent_versions":  []string{"v1", "v2", "hybrid"},
		"selection_presets": []string{"all", "video"},
		"operations": []string{"plan", "create", "list", "get", "files", "select_files", "pause", "resume",
			"set_limits", "remove_preview", "remove"},
		"state_storage":               "none", // every read is live from the engine
		"all_engine_torrents":         true,   // WebUI-added torrents are listed and controllable
		"file_content":                s.cfg.Files.ContentAccess,
		"webui":                       s.cfg.WebUI.Enabled,
		"streaming_while_downloading": false,
		"metadata_only_guarantee":     false,
		"limits": map[string]any{
			"max_active_tasks": l.MaxActiveTasks, "max_task_bytes": l.MaxTaskBytes,
			"max_torrent_file_bytes": l.MaxTorrentFileBytes, "max_download_rate": l.MaxDownloadRate,
			"max_upload_rate": l.MaxUploadRate, "min_free_bytes": l.MinFreeBytes,
			"plan_ttl_seconds": l.PlanTTLSeconds,
		},
		"seeding":      map[string]any{"ratio_limit": s.cfg.Seeding.RatioLimit, "seeding_time_minutes": s.cfg.Seeding.SeedingTimeMinutes},
		"destinations": s.Destinations(),
	}
}
