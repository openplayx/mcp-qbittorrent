package torrents

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/openplayx/bittorrent-server/src/internal/qbittorrent"
)

// ---- uploaded .torrent inputs ----

type InputView struct {
	InputRef       string    `json:"input_ref"`
	Name           string    `json:"name"`
	TorrentVersion string    `json:"torrent_version"`
	FileCount      int       `json:"file_count"`
	TotalSize      int64     `json:"total_size"`
	Private        bool      `json:"private"`
	Removed        *Removed  `json:"removed_addresses,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func (s *Service) CreateInput(ctx context.Context, caller string, data []byte) (InputView, error) {
	if int64(len(data)) > s.cfg.Limits.MaxTorrentFileBytes {
		return InputView{}, fail(http.StatusRequestEntityTooLarge, "torrent_too_large")
	}
	in, err := ParseTorrent(ctx, data, s.policy)
	if err != nil {
		return InputView{}, inputErr(err)
	}
	ttl := time.Duration(s.cfg.Limits.InputTTLSeconds) * time.Second
	// Keep the sanitised copy: disallowed trackers never reach the engine.
	id := s.inputs.put(caller, in.Torrent, ttl)
	v := InputView{
		InputRef: id, Name: in.Name, TorrentVersion: in.Identity.Version(), FileCount: len(in.Files),
		TotalSize: in.Size, Private: in.Private, ExpiresAt: time.Now().Add(ttl).UTC(),
	}
	if in.Removed.Total() > 0 {
		r := in.Removed
		v.Removed = &r
	}
	return v, nil
}

func inputErr(err error) error {
	var ie *InputError
	if errors.As(err, &ie) {
		return fail(http.StatusUnprocessableEntity, ie.Code)
	}
	return err
}

// ---- plans (signed tokens, nothing stored) ----

type PlanSource struct {
	Type     string `json:"type"` // "magnet" | "input"
	Magnet   string `json:"magnet,omitempty"`
	InputRef string `json:"input_ref,omitempty"`
}

type PlanRequest struct {
	Source          PlanSource `json:"source"`
	DestinationRef  string     `json:"destination_ref"`
	SelectionPreset string     `json:"selection_preset"`
	AutoStart       *bool      `json:"auto_start"`
	DownloadLimit   int64      `json:"download_limit"`
	UploadLimit     int64      `json:"upload_limit"`
}

type planToken struct {
	Kind        string   `json:"k"`
	Identity    Identity `json:"i"`
	Name        string   `json:"n"`
	Magnet      string   `json:"m,omitempty"`
	InputRef    string   `json:"r,omitempty"`
	Caller      string   `json:"c"`
	Destination string   `json:"d"`
	DestPath    string   `json:"dp"`
	Preset      string   `json:"p"`
	AutoStart   bool     `json:"a"`
	DownLimit   int64    `json:"dl"`
	UpLimit     int64    `json:"ul"`
	Expires     int64    `json:"e"`
}

type PlanView struct {
	PlanRef         string          `json:"plan_ref"`
	ExpiresAt       time.Time       `json:"expires_at"`
	InputKind       string          `json:"input_kind"`
	Name            string          `json:"name,omitempty"`
	TorrentVersion  string          `json:"torrent_version"`
	Metadata        string          `json:"metadata"` // known | unknown
	TotalSize       *int64          `json:"total_size,omitempty"`
	SelectedSize    *int64          `json:"selected_size,omitempty"`
	FileCount       *int            `json:"file_count,omitempty"`
	Files           []PlanFile      `json:"files_preview,omitempty"`
	Destination     DestinationView `json:"destination"`
	SelectionPreset string          `json:"selection_preset"`
	AutoStart       bool            `json:"auto_start"`
	DownloadLimit   int64           `json:"download_limit"`
	UploadLimit     int64           `json:"upload_limit"`
	Trackers        int             `json:"trackers"`
	Removed         *Removed        `json:"removed_addresses,omitempty"`
	Warnings        []string        `json:"warnings"`
}

type PlanFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Selected bool   `json:"selected"`
}

const planPrefix = "plan."

func (s *Service) CreatePlan(ctx context.Context, caller string, req PlanRequest) (PlanView, error) {
	dest, ok := s.cfg.Destination(req.DestinationRef)
	if !ok {
		return PlanView{}, fail(http.StatusUnprocessableEntity, "destination_not_allowed")
	}
	if req.SelectionPreset == "" {
		req.SelectionPreset = "all"
	}
	if req.SelectionPreset != "all" && req.SelectionPreset != "video" {
		return PlanView{}, fail(http.StatusUnprocessableEntity, "invalid_selection_preset")
	}
	dl, okd := clampRate(req.DownloadLimit, s.cfg.Limits.MaxDownloadRate)
	ul, oku := clampRate(req.UploadLimit, s.cfg.Limits.MaxUploadRate)
	if !okd || !oku {
		return PlanView{}, fail(http.StatusUnprocessableEntity, "limit_exceeds_maximum")
	}
	var in *Input
	var err error
	switch req.Source.Type {
	case "magnet":
		if req.Source.InputRef != "" {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "mixed_input")
		}
		in, err = ParseMagnet(ctx, req.Source.Magnet, s.cfg.Limits.MaxMagnetLength, s.policy)
	case "input":
		if req.Source.Magnet != "" {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "mixed_input")
		}
		data, found := s.inputs.get(caller, req.Source.InputRef)
		if !found {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "input_expired")
		}
		in, err = ParseTorrent(ctx, data, s.policy)
	default:
		return PlanView{}, fail(http.StatusUnprocessableEntity, "unsupported_input")
	}
	if err != nil {
		return PlanView{}, inputErr(err)
	}
	if t, found, err := s.findIdentity(ctx, in.Identity); err != nil {
		return PlanView{}, err
	} else if found {
		return PlanView{}, &Error{Status: http.StatusConflict, Code: "duplicate_task", Detail: map[string]any{"task_id": t.Hash}}
	}
	autoStart := req.AutoStart == nil || *req.AutoStart
	view := PlanView{
		InputKind: in.Kind, Name: in.Name, TorrentVersion: in.Identity.Version(), Metadata: "unknown",
		SelectionPreset: req.SelectionPreset, AutoStart: autoStart, DownloadLimit: dl, UploadLimit: ul,
		Warnings: []string{}, Destination: s.destinationView(dest.Ref),
	}
	if in.Kind == "magnet" {
		// No peers are contacted for a preview; discovery starts on create.
		view.Warnings = append(view.Warnings, "metadata_unknown", "metadata_discovery_contacts_peers")
	} else {
		sel, selSize := selectPreset(in.Files, req.SelectionPreset)
		total, count := in.Size, len(in.Files)
		view.Metadata, view.TotalSize, view.SelectedSize, view.FileCount = "known", &total, &selSize, &count
		for i, f := range in.Files {
			if i == 50 {
				view.Warnings = append(view.Warnings, "files_preview_truncated")
				break
			}
			view.Files = append(view.Files, PlanFile{Path: f.Path, Size: f.Size, Selected: sel[i]})
		}
		if selSize == 0 {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "no_files_match_preset")
		}
		if selSize > s.maxTaskBytes(s.cfg.DestinationPath(dest)) {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "quota_exceeded")
		}
		if free := view.Destination.FreeBytes; free != nil && selSize+s.cfg.Limits.MinFreeBytes > *free {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "insufficient_disk_space")
		}
	}
	if in.Private {
		view.Warnings = append(view.Warnings, "private_torrent")
	}
	view.Trackers = in.Trackers
	if in.Removed.Total() > 0 {
		r := in.Removed
		view.Removed = &r
		view.Warnings = append(view.Warnings, "addresses_removed")
	}
	if in.Trackers == 0 && in.Kind == "magnet" {
		view.Warnings = append(view.Warnings, "no_trackers_dht_only")
	}
	expires := time.Now().Add(time.Duration(s.cfg.Limits.PlanTTLSeconds) * time.Second)
	view.PlanRef = s.seal(planPrefix, planToken{
		Kind: in.Kind, Identity: in.Identity, Name: in.Name, Magnet: in.Magnet, InputRef: req.Source.InputRef,
		Caller: caller, Destination: dest.Ref, DestPath: s.cfg.DestinationPath(dest), Preset: req.SelectionPreset,
		AutoStart: autoStart, DownLimit: dl, UpLimit: ul, Expires: expires.Unix(),
	})
	view.ExpiresAt = expires.UTC()
	return view, nil
}

func selectPreset(files []PreviewFile, preset string) ([]bool, int64) {
	sel := make([]bool, len(files))
	var size int64
	for i, f := range files {
		if preset == "all" || (preset == "video" && IsVideo(f.Path)) {
			sel[i] = true
			size += f.Size
		}
	}
	return sel, size
}

// findIdentity looks the payload up in the engine (the only authority).
func (s *Service) findIdentity(ctx context.Context, id Identity) (qbittorrent.Torrent, bool, error) {
	list, err := s.engine.Info(ctx, "")
	if err != nil {
		return qbittorrent.Torrent{}, false, engineErr(err)
	}
	for _, t := range list {
		if id.Matches(t.InfohashV1, t.InfohashV2) {
			return t, true, nil
		}
	}
	return qbittorrent.Torrent{}, false, nil
}

// ---- create ----

type CreateRequest struct {
	PlanRef string `json:"plan_ref"`
}

// CreateTask adds the planned torrent and returns once the engine lists it.
// Repeating it with the same plan returns the existing torrent (created =
// false): the engine's own identity check makes creation idempotent.
func (s *Service) CreateTask(ctx context.Context, caller, requestID string, req CreateRequest) (TaskView, bool, error) {
	var p planToken
	if !s.unseal(planPrefix, req.PlanRef, &p) || p.Caller != caller {
		return TaskView{}, false, fail(http.StatusUnprocessableEntity, "plan_invalid")
	}
	if time.Now().Unix() >= p.Expires {
		return TaskView{}, false, fail(http.StatusConflict, "plan_expired")
	}
	dest, ok := s.cfg.Destination(p.Destination)
	if !ok || s.cfg.DestinationPath(dest) != p.DestPath {
		return TaskView{}, false, fail(http.StatusConflict, "plan_invalidated")
	}
	list, err := s.engine.Info(ctx, "")
	if err != nil {
		return TaskView{}, false, engineErr(err)
	}
	for _, t := range list {
		if p.Identity.Matches(t.InfohashV1, t.InfohashV2) {
			return s.viewLive(ctx, t), false, nil
		}
	}
	if len(list) >= s.cfg.Limits.MaxActiveTasks {
		return TaskView{}, false, fail(http.StatusTooManyRequests, "task_limit_reached")
	}
	var torrent []byte
	if p.Kind == "torrent_file" {
		data, found := s.inputs.get(caller, p.InputRef)
		if !found {
			return TaskView{}, false, fail(http.StatusConflict, "input_expired")
		}
		torrent = data
	}
	dir := filepath.Join(p.DestPath, "opx-"+randomID(8))
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return TaskView{}, false, fail(http.StatusServiceUnavailable, "destination_unavailable")
	}
	tags := TagService + "," + TagPending
	if p.AutoStart {
		tags += "," + TagAuto
	}
	if p.Preset == "video" {
		tags += "," + TagPresetVideo
	}
	add := qbittorrent.AddRequest{
		SavePath: dir, Tag: tags, DownloadLimit: p.DownLimit, UploadLimit: p.UpLimit,
		RatioLimit: s.cfg.Seeding.RatioLimit, SeedingTime: s.cfg.Seeding.SeedingTimeMinutes,
	}
	if p.Kind == "magnet" {
		// Metadata-first: the engine stops once metadata arrives so the
		// selection is applied before payload transfer.
		add.Magnet, add.StopCondition = p.Magnet, "MetadataReceived"
	} else {
		add.Torrent, add.Stopped = torrent, true
	}
	addErr := s.engine.Add(ctx, add)
	if addErr != nil && !qbittorrent.IsCode(addErr, qbittorrent.CodeUnavailable) {
		_ = os.Remove(dir)
		return TaskView{}, false, engineErr(addErr)
	}
	// A lost reply is reconciled against the engine, never blindly retried.
	var t qbittorrent.Torrent
	deadline := time.Now().Add(s.ConfirmTimeout)
	for {
		if list, err := s.engine.Info(ctx, TagService); err == nil {
			found := false
			for _, x := range list {
				if p.Identity.Matches(x.InfohashV1, x.InfohashV2) {
					t, found = x, true
				}
			}
			if found {
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			_ = os.Remove(dir)
			if addErr != nil {
				return TaskView{}, false, engineErr(addErr)
			}
			return TaskView{}, false, &Error{Status: http.StatusGatewayTimeout, Code: "creation_unconfirmed", Retryable: true}
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.ConfirmPoll):
		}
	}
	s.log.Info("torrent.create", "request", requestID, "caller", caller, "task", t.Hash, "kind", p.Kind)
	if t.HasMetadata {
		unlock := s.lock(t.Hash)
		s.applyInitialSelection(ctx, t, s.free())
		unlock()
		if fresh, err := s.get(ctx, t.Hash); err == nil {
			t = fresh
		}
	}
	return s.viewLive(ctx, t), true, nil
}

func (s *Service) free() int64 {
	if s.freeBytes == nil {
		return -1
	}
	f, err := s.freeBytes(s.cfg.DownloadRoot)
	if err != nil {
		return -1
	}
	return f
}

// applyInitialSelection runs once metadata is known for a torrent carrying
// TagPending: selects by preset, enforces quota and disk space, then starts
// it or records a hold tag. The pending tag is removed last, so a failure
// is retried by the next reconcile.
func (s *Service) applyInitialSelection(ctx context.Context, t qbittorrent.Torrent, free int64) {
	tags := tagSet(t)
	files, err := s.engine.Files(ctx, t.Hash)
	if err != nil || len(files) == 0 {
		return
	}
	var on, off []int
	var size int64
	for _, f := range files {
		if !tags[TagPresetVideo] || IsVideo(f.Name) {
			on = append(on, f.Index)
			size += f.Size
		} else {
			off = append(off, f.Index)
		}
	}
	hold := ""
	switch {
	case len(on) == 0:
		hold = TagHoldNoMatch
	case size > s.maxTaskBytes(t.SavePath):
		hold = TagHoldQuota
	case free >= 0 && size+s.cfg.Limits.MinFreeBytes > free:
		hold = TagHoldDisk
	case !tags[TagAuto]:
		hold = TagHoldStart
	}
	if err := s.engine.SetFilePriority(ctx, t.Hash, off, 0); err != nil {
		return
	}
	if err := s.engine.SetFilePriority(ctx, t.Hash, on, 1); err != nil {
		return
	}
	if hold != "" {
		if !isStopped(t.State) {
			_ = s.engine.Stop(ctx, t.Hash)
		}
		if err := s.engine.AddTags(ctx, t.Hash, hold); err != nil {
			return
		}
	} else if err := s.engine.Start(ctx, t.Hash); err != nil {
		return
	}
	if err := s.engine.RemoveTags(ctx, t.Hash, TagPending); err == nil {
		s.log.Info("selection applied", "task", t.Hash, "files", len(on), "hold", hold)
	}
}

func mkdirAll(path string) error { return os.MkdirAll(path, 0o775) }
