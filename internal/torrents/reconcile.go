package torrents

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/openplayx/bittorrent-server/internal/qbittorrent"
)

type qbTorrent = qbittorrent.Torrent

func toSnapFiles(fs []qbittorrent.File) []SnapshotFile {
	out := make([]SnapshotFile, len(fs))
	for i, f := range fs {
		out[i] = SnapshotFile{Index: f.Index, Name: f.Name, Size: f.Size, Progress: f.Progress, Priority: f.Priority}
	}
	return out
}

// Recover resolves operations interrupted by a crash or restart. Creation
// is matched against the engine; nothing is re-added automatically.
func (s *Service) Recover(ctx context.Context) error {
	ops, err := s.store.PendingOperations(ctx)
	if err != nil {
		return err
	}
	list, engErr := s.engine.Info(ctx, ServiceTag)
	for _, op := range ops {
		if op.Kind == "create" && op.TaskID != "" && engErr == nil {
			row, err := s.store.GetTaskAny(ctx, op.TaskID)
			if err != nil {
				continue
			}
			r, err := decodeTask(row)
			if err != nil {
				continue
			}
			id := Identity{V1: row.V1, V2: row.V2}
			matched := false
			for _, t := range list {
				if id.Matches(t.InfohashV1, t.InfohashV2) && strings.Contains(t.Tags, r.body.Tag) {
					r.body.EngineHash = t.Hash
					_ = s.saveTask(ctx, r, "active")
					_ = s.store.FinishOperation(ctx, op.ID, "succeeded", "", false, map[string]any{"task_id": op.TaskID})
					matched = true
				}
			}
			if !matched {
				r.body.Error = "creation_interrupted"
				_ = s.saveTask(ctx, r, "failed")
				_ = s.store.FinishOperation(ctx, op.ID, "failed", "creation_interrupted", false, map[string]any{"task_id": op.TaskID})
			}
			continue
		}
		// The caller re-reads state and decides; mutations are not replayed.
		_ = s.store.FinishOperation(ctx, op.ID, "failed", "operation_interrupted", true, map[string]any{"task_id": op.TaskID})
	}
	return nil
}

// Run reconciles periodically until ctx ends.
func (s *Service) Run(ctx context.Context) {
	interval := time.Duration(s.cfg.ReconcileIntervalSeconds) * time.Second
	retention := time.Duration(s.cfg.RetentionDays) * 24 * time.Hour
	lastPrune := time.Time{}
	for {
		s.Reconcile(ctx)
		if retention > 0 && time.Since(lastPrune) > time.Hour {
			_ = s.store.Prune(ctx, retention)
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Reconcile projects engine state onto every live task once.
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
	tasks, err := s.store.ActiveTasks(ctx)
	if err != nil {
		return
	}
	known := map[string]bool{}
	var free int64 = -1
	if s.freeBytes != nil {
		if f, err := s.freeBytes(s.cfg.DownloadRoot); err == nil {
			free = f
		}
	}
	diskLow := free >= 0 && free < s.cfg.Limits.MinFreeBytes
	diskOK := free < 0 || free >= s.cfg.Limits.MinFreeBytes+s.cfg.Limits.MinFreeBytes/10
	for _, row := range tasks {
		if row.Phase != "active" {
			continue
		}
		s.reconcileTask(ctx, row.ID, list, diskLow, diskOK, known)
	}
	for _, t := range list {
		if strings.Contains(t.Tags, "opx-") && !known[t.Hash] {
			// Created by this service but no longer bound (e.g. after a
			// failed creation): reported, never adopted or modified.
			s.log.Debug("unbound engine torrent", "name_len", len(t.Name))
		}
	}
}

func (s *Service) reconcileTask(ctx context.Context, id string, list []qbTorrent, diskLow, diskOK bool, known map[string]bool) {
	unlock := s.lock(id)
	defer unlock()
	row, err := s.store.GetTaskAny(ctx, id)
	if err != nil || row.Phase != "active" {
		return
	}
	r, err := decodeTask(row)
	if err != nil {
		return
	}
	var t *qbTorrent
	for i := range list {
		if list[i].Hash == r.body.EngineHash || (Identity{V1: row.V1, V2: row.V2}).Matches(list[i].InfohashV1, list[i].InfohashV2) {
			t = &list[i]
			break
		}
	}
	if t == nil {
		// Removed outside the service, or engine state lost. Report it;
		// never silently re-add.
		snap := Snapshot{Missing: true}
		if r.snap != nil {
			snap = *r.snap
			snap.Missing = true
			snap.DlSpeed, snap.UpSpeed = 0, 0
		}
		if r.body.MissingSince == 0 {
			r.body.MissingSince = time.Now().Unix()
			_ = s.saveTask(ctx, r, "active")
			s.log.Warn("engine task missing", "task", id)
		}
		_ = s.saveSnapshot(ctx, r, &snap)
		return
	}
	known[t.Hash] = true
	if r.body.MissingSince != 0 {
		r.body.MissingSince = 0
		_ = s.saveTask(ctx, r, "active")
	}
	if r.body.EngineHash == "" {
		r.body.EngineHash = t.Hash
	}
	snap := &Snapshot{
		EngineState: t.State, HasMetadata: t.HasMetadata, Progress: t.Progress, Size: t.Size,
		TotalSize: t.TotalSize, Completed: t.Completed, Downloaded: t.Downloaded, Uploaded: t.Uploaded,
		DlSpeed: t.DlSpeed, UpSpeed: t.UpSpeed, DlLimit: t.DlLimit, UpLimit: t.UpLimit,
		Seeds: t.NumSeeds, Peers: t.NumLeechs, Ratio: t.Ratio, ETA: t.ETA,
	}
	if t.HasMetadata {
		if fs, err := s.engine.Files(ctx, t.Hash); err == nil {
			snap.Files = toSnapFiles(fs)
		} else if r.snap != nil {
			snap.Files = r.snap.Files
		}
	}
	if !t.HasMetadata && t.Size == 0 {
		snap.Size, snap.TotalSize = 0, 0
	}
	_ = s.saveSnapshot(ctx, r, snap)

	// Metadata arrived: apply the plan's selection before payload transfer.
	if t.HasMetadata && !r.body.SelectionDone && len(snap.Files) > 0 {
		s.applyInitialSelection(ctx, r)
		return
	}
	// Continuous disk watermark protection, not just a plan-time check.
	downloading := !isStopped(t.State) && t.Progress < 1
	switch {
	case diskLow && downloading:
		if err := s.engine.Stop(ctx, t.Hash); err == nil {
			r.body.DiskPaused, r.body.HoldReason = true, "disk_low"
			_ = s.saveTask(ctx, r, "active")
			s.log.Warn("paused for low disk", "task", id)
		}
	case r.body.DiskPaused && diskOK:
		r.body.DiskPaused, r.body.HoldReason = false, ""
		if !r.body.UserPaused && r.body.AutoStart {
			_ = s.engine.Start(ctx, t.Hash)
		}
		_ = s.saveTask(ctx, r, "active")
	}
}

func (s *Service) applyInitialSelection(ctx context.Context, r *taskRec) {
	sel := map[int]bool{}
	var size int64
	for _, f := range r.snap.Files {
		if r.body.SelectionPreset != "video" || IsVideo(f.Name) {
			sel[f.Index] = true
			size += f.Size
		}
	}
	if len(sel) == 0 {
		// Nothing matched: keep everything deselected-by-default off and
		// wait for an explicit selection.
		for _, f := range r.snap.Files {
			sel[f.Index] = false
		}
		if err := s.applySelection(ctx, r, sel); err != nil {
			return
		}
		r.body.HoldReason = "no_files_match_preset"
		_ = s.saveTask(ctx, r, "active")
		return
	}
	if err := s.applySelection(ctx, r, sel); err != nil {
		s.log.Warn("apply selection failed", "task", r.row.ID, "err", err)
		return
	}
	switch {
	case size > s.maxTaskBytes(r.body.Destination):
		r.body.HoldReason = "quota_exceeded"
	case s.freeBytes != nil && func() bool {
		free, err := s.freeBytes(filepath.Dir(r.body.SavePath))
		return err == nil && size+s.cfg.Limits.MinFreeBytes > free
	}():
		r.body.HoldReason = "disk_low"
		r.body.DiskPaused = true
	case !r.body.AutoStart || r.body.UserPaused:
		if !r.body.UserPaused {
			r.body.HoldReason = "awaiting_selection"
		}
	default:
		if err := s.engine.Start(ctx, r.body.EngineHash); err != nil {
			s.log.Warn("start after metadata failed", "task", r.row.ID)
			_ = s.saveTask(ctx, r, "active")
			return
		}
		r.body.Started = true
	}
	_ = s.saveTask(ctx, r, "active")
	s.log.Info("selection applied", "task", r.row.ID, "files", len(sel), "hold", r.body.HoldReason)
}

// Health is served by /readyz and capabilities.
type Health struct {
	Database      bool   `json:"database"`
	Engine        bool   `json:"engine"`
	EngineVersion string `json:"engine_version,omitempty"`
	WebAPIVersion string `json:"webapi_version,omitempty"`
	LastEngineAt  *int64 `json:"last_engine_contact,omitempty"`
}

func (s *Service) Health(ctx context.Context) Health {
	h := Health{Database: s.store.Ping(ctx) == nil, Engine: s.healthy.Load()}
	h.EngineVersion, h.WebAPIVersion = s.engine.Versions()
	if v := s.lastSeen.Load(); v > 0 {
		h.LastEngineAt = &v
	}
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
	v := DestinationView{Ref: d.Ref, Name: d.Name, MaxTaskBytes: s.maxTaskBytes(d.Ref)}
	if s.freeBytes != nil {
		path := s.cfg.DestinationPath(d)
		_ = mkdirTask(path)
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
		"engine":            map[string]any{"name": "qBittorrent", "version": h.EngineVersion, "webapi_version": h.WebAPIVersion, "available": h.Engine, "instance": s.instance},
		"inputs":            []string{"magnet", "torrent_file"},
		"torrent_versions":  []string{"v1", "v2", "hybrid"},
		"selection_presets": []string{"all", "video"},
		"operations": []string{"plan", "create", "list", "get", "files", "select_files", "pause", "resume",
			"set_limits", "remove_preview", "remove"},
		"file_content":                s.cfg.Files.ContentAccess,
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

var _ = json.Marshal
