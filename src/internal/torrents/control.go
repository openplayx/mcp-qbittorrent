package torrents

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openplayx/bittorrent-server/src/internal/files"
	"github.com/openplayx/bittorrent-server/src/internal/qbittorrent"
)

// ---- reads (always live from the engine) ----

func (s *Service) ListTasks(ctx context.Context) ([]TaskView, error) {
	list, err := s.engine.Info(ctx, "")
	if err != nil {
		return nil, engineErr(err)
	}
	out := make([]TaskView, 0, len(list))
	for _, t := range list {
		out = append(out, s.viewLive(ctx, t))
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].AddedAt, out[j].AddedAt
		if ai == nil || aj == nil || ai.Equal(*aj) {
			return out[i].Name < out[j].Name
		}
		return ai.After(*aj)
	})
	return out, nil
}

func (s *Service) GetTask(ctx context.Context, id string) (TaskView, error) {
	t, err := s.get(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	return s.viewLive(ctx, t), nil
}

func (s *Service) Files(ctx context.Context, id string) (FilesView, error) {
	t, err := s.get(ctx, id)
	if err != nil {
		return FilesView{}, err
	}
	v := FilesView{TaskID: id, Files: []FileView{}}
	if !t.HasMetadata {
		return v, nil
	}
	fs, err := s.engine.Files(ctx, id)
	if err != nil {
		return FilesView{}, engineErr(err)
	}
	v.MetadataKnown, v.FilesRevision = true, filesRevision(fs)
	for _, f := range fs {
		complete := f.Progress >= 1
		v.Files = append(v.Files, FileView{
			ID: fileID(f.Index), Path: f.Name, Size: f.Size, Progress: f.Progress,
			Selected: f.Priority > 0, Complete: complete, IsVideo: IsVideo(f.Name),
			Available: s.cfg.Files.ContentAccess && complete,
		})
	}
	return v, nil
}

// ---- mutations: applied to the engine and confirmed from its state ----

func (s *Service) Pause(ctx context.Context, caller, requestID, id string) (TaskView, error) {
	t, err := s.get(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	defer s.lock(id)()
	// A user pause overrides automatic disk resumption.
	if tagSet(t)[TagHoldDisk] {
		_ = s.engine.RemoveTags(ctx, id, TagHoldDisk)
	}
	if err := s.engine.Stop(ctx, id); err != nil {
		return TaskView{}, engineErr(err)
	}
	t, err = s.waitFor(ctx, id, func(t qbittorrent.Torrent, ok bool) bool { return !ok || isStopped(t.State) })
	if err != nil {
		return TaskView{}, err
	}
	s.log.Info("torrent.pause", "request", requestID, "caller", caller, "task", id)
	return s.viewLive(ctx, t), nil
}

// Resume restarts a torrent (never force-start). Holds are re-checked
// against the current selection and disk space instead of trusted.
func (s *Service) Resume(ctx context.Context, caller, requestID, id string) (TaskView, error) {
	t, err := s.get(ctx, id)
	if err != nil {
		return TaskView{}, err
	}
	defer s.lock(id)()
	tags := tagSet(t)
	if tags[TagHoldNoMatch] {
		return TaskView{}, fail(http.StatusConflict, "no_files_match_preset")
	}
	if t.HasMetadata {
		if t.Size > s.maxTaskBytes(t.SavePath) {
			return TaskView{}, fail(http.StatusConflict, "quota_exceeded")
		}
		if free := s.free(); free >= 0 && nonNegative(t.AmountLeft)+s.cfg.Limits.MinFreeBytes > free {
			return TaskView{}, fail(http.StatusConflict, "insufficient_disk_space")
		}
	}
	var clear []string
	for _, tag := range []string{TagHoldQuota, TagHoldDisk, TagHoldStart} {
		if tags[tag] {
			clear = append(clear, tag)
		}
	}
	if len(clear) > 0 {
		if err := s.engine.RemoveTags(ctx, id, clear...); err != nil {
			return TaskView{}, engineErr(err)
		}
	}
	if err := s.engine.Start(ctx, id); err != nil {
		return TaskView{}, engineErr(err)
	}
	t, err = s.waitFor(ctx, id, func(t qbittorrent.Torrent, ok bool) bool { return ok && !isStopped(t.State) })
	if err != nil {
		return TaskView{}, err
	}
	s.log.Info("torrent.resume", "request", requestID, "caller", caller, "task", id)
	return s.viewLive(ctx, t), nil
}

type LimitsRequest struct {
	DownloadLimit int64 `json:"download_limit"`
	UploadLimit   int64 `json:"upload_limit"`
}

func (s *Service) SetLimits(ctx context.Context, caller, requestID, id string, req LimitsRequest) (TaskView, error) {
	dl, okd := clampRate(req.DownloadLimit, s.cfg.Limits.MaxDownloadRate)
	ul, oku := clampRate(req.UploadLimit, s.cfg.Limits.MaxUploadRate)
	if !okd || !oku {
		return TaskView{}, fail(http.StatusUnprocessableEntity, "limit_exceeds_maximum")
	}
	if _, err := s.get(ctx, id); err != nil {
		return TaskView{}, err
	}
	defer s.lock(id)()
	if err := s.engine.SetLimits(ctx, id, dl, ul); err != nil {
		return TaskView{}, engineErr(err)
	}
	t, err := s.waitFor(ctx, id, func(t qbittorrent.Torrent, ok bool) bool {
		return ok && nonNegative(t.DlLimit) == dl && nonNegative(t.UpLimit) == ul
	})
	if err != nil {
		return TaskView{}, err
	}
	s.log.Info("torrent.limits", "request", requestID, "caller", caller, "task", id)
	return s.viewLive(ctx, t), nil
}

type SelectionRequest struct {
	ExpectedRevision string   `json:"expected_revision"`
	Preset           string   `json:"preset,omitempty"`
	FileIDs          []string `json:"file_ids,omitempty"`
}

func (s *Service) SetSelection(ctx context.Context, caller, requestID, id string, req SelectionRequest) (FilesView, error) {
	if (req.Preset == "") == (req.FileIDs == nil) {
		return FilesView{}, fail(http.StatusUnprocessableEntity, "preset_or_file_ids_required")
	}
	if req.Preset != "" && req.Preset != "all" && req.Preset != "video" {
		return FilesView{}, fail(http.StatusUnprocessableEntity, "invalid_selection_preset")
	}
	t, err := s.get(ctx, id)
	if err != nil {
		return FilesView{}, err
	}
	if !t.HasMetadata {
		return FilesView{}, fail(http.StatusConflict, "metadata_pending")
	}
	defer s.lock(id)()
	fs, err := s.engine.Files(ctx, id)
	if err != nil {
		return FilesView{}, engineErr(err)
	}
	if req.ExpectedRevision != filesRevision(fs) {
		return FilesView{}, fail(http.StatusConflict, "files_revision_mismatch")
	}
	sel := map[int]bool{}
	known := map[int]bool{}
	var size int64
	for _, f := range fs {
		known[f.Index] = true
		if req.Preset == "all" || (req.Preset == "video" && IsVideo(f.Name)) {
			sel[f.Index] = true
		}
	}
	for _, fid := range req.FileIDs {
		idx, ok := parseFileID(fid)
		if !ok || !known[idx] {
			return FilesView{}, fail(http.StatusUnprocessableEntity, "unknown_file_id")
		}
		sel[idx] = true
	}
	if len(sel) == 0 {
		return FilesView{}, fail(http.StatusUnprocessableEntity, "empty_selection")
	}
	var on, off []int
	for _, f := range fs {
		if sel[f.Index] {
			on = append(on, f.Index)
			size += f.Size
		} else {
			off = append(off, f.Index)
		}
	}
	if size > s.maxTaskBytes(t.SavePath) {
		return FilesView{}, fail(http.StatusUnprocessableEntity, "quota_exceeded")
	}
	if err := s.engine.SetFilePriority(ctx, id, off, 0); err != nil {
		return FilesView{}, engineErr(err)
	}
	if err := s.engine.SetFilePriority(ctx, id, on, 1); err != nil {
		return FilesView{}, engineErr(err)
	}
	got, err := s.engine.Files(ctx, id)
	if err != nil {
		return FilesView{}, engineErr(err)
	}
	for _, f := range got {
		if (f.Priority > 0) != sel[f.Index] {
			return FilesView{}, &Error{Status: http.StatusGatewayTimeout, Code: "engine_state_unconfirmed", Retryable: true}
		}
	}
	// A selection that now fits lifts a quota / no-match hold; starting it
	// stays an explicit resume.
	tags := tagSet(t)
	if tags[TagHoldQuota] || tags[TagHoldNoMatch] {
		_ = s.engine.RemoveTags(ctx, id, TagHoldQuota, TagHoldNoMatch)
		_ = s.engine.AddTags(ctx, id, TagHoldStart)
	}
	s.log.Info("torrent.selection", "request", requestID, "caller", caller, "task", id, "files", len(on))
	return s.Files(ctx, id)
}

// ---- removal: preview → signed confirmation → apply ----

type RemovePlanRequest struct {
	DeleteFiles bool `json:"delete_files"`
}

type removeToken struct {
	ID          string `json:"i"`
	DeleteFiles bool   `json:"d"`
	AddedOn     int64  `json:"a"`
	Expires     int64  `json:"e"`
}

const removePrefix = "rm."

type RemovePlanView struct {
	PlanRef      string    `json:"plan_ref"`
	TaskID       string    `json:"task_id"`
	TaskName     string    `json:"task_name"`
	DeleteFiles  bool      `json:"delete_files"`
	FilesOnDisk  int       `json:"files_on_disk"`
	BytesOnDisk  int64     `json:"bytes_on_disk"`
	AddedVia     string    `json:"added_via"`
	StopsSeeding bool      `json:"stops_seeding"`
	Effects      []string  `json:"effects"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *Service) RemovePlan(ctx context.Context, id string, req RemovePlanRequest) (RemovePlanView, error) {
	t, err := s.get(ctx, id)
	if err != nil {
		return RemovePlanView{}, err
	}
	v := RemovePlanView{
		TaskID: id, TaskName: safeDisplayName(t.Name), DeleteFiles: req.DeleteFiles,
		AddedVia: s.view(t).AddedVia, StopsSeeding: mapEngineState(t.State, true) == StateSeeding,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	}
	if t.HasMetadata {
		if fs, err := s.engine.Files(ctx, id); err == nil {
			for _, f := range fs {
				if f.Progress > 0 {
					v.FilesOnDisk++
					v.BytesOnDisk += int64(float64(f.Size) * f.Progress)
				}
			}
		}
	}
	v.Effects = []string{"engine_task_removed"}
	if req.DeleteFiles {
		v.Effects = append(v.Effects, "downloaded_files_deleted")
	} else {
		v.Effects = append(v.Effects, "downloaded_files_retained")
	}
	if v.StopsSeeding {
		v.Effects = append(v.Effects, "seeding_stops")
	}
	// The token binds the torrent, the choice and the torrent's identity in
	// time (added_on), so it cannot be replayed onto a re-added torrent.
	v.PlanRef = s.seal(removePrefix, removeToken{ID: id, DeleteFiles: req.DeleteFiles, AddedOn: t.AddedOn, Expires: v.ExpiresAt.Unix()})
	return v, nil
}

type RemoveRequest struct {
	PlanRef string `json:"plan_ref"`
}

type RemoveResult struct {
	TaskID      string `json:"task_id"`
	Removed     bool   `json:"removed"`
	DataDeleted bool   `json:"data_deleted"`
}

func (s *Service) Remove(ctx context.Context, caller, requestID, id string, req RemoveRequest) (RemoveResult, error) {
	var p removeToken
	if !s.unseal(removePrefix, req.PlanRef, &p) || p.ID != id {
		return RemoveResult{}, fail(http.StatusUnprocessableEntity, "remove_plan_invalid")
	}
	if time.Now().Unix() >= p.Expires {
		return RemoveResult{}, fail(http.StatusConflict, "remove_plan_expired")
	}
	t, err := s.get(ctx, id)
	var be *Error
	if errors.As(err, &be) && be.Code == "task_not_found" {
		return RemoveResult{TaskID: id, Removed: true}, nil // already gone
	}
	if err != nil {
		return RemoveResult{}, err
	}
	if t.AddedOn != p.AddedOn {
		return RemoveResult{}, fail(http.StatusConflict, "task_changed_since_plan")
	}
	defer s.lock(id)()
	if err := s.engine.Delete(ctx, id, p.DeleteFiles); err != nil {
		return RemoveResult{}, engineErr(err)
	}
	if _, err := s.waitFor(ctx, id, func(_ qbittorrent.Torrent, ok bool) bool { return !ok }); err != nil {
		return RemoveResult{}, err
	}
	// Clean up the per-task directory this API created, once it is empty.
	if p.DeleteFiles {
		if _, ok := s.destinationOf(t.SavePath); ok && strings.HasPrefix(filepath.Base(t.SavePath), "opx-") {
			// The engine deletes large files asynchronously; tidy up the
			// empty directory in the background once it has finished.
			go s.removeEmptyDirs(t.SavePath, 2*time.Minute)
		}
	}
	s.log.Info("torrent.remove", "request", requestID, "caller", caller, "task", id, "delete_files", p.DeleteFiles)
	return RemoveResult{TaskID: id, Removed: true, DataDeleted: p.DeleteFiles}, nil
}

// removeEmptyDirs deletes empty directories bottom-up below dir, retrying
// until within (the engine may still be deleting files; non-empty
// directories are never removed).
func (s *Service) removeEmptyDirs(dir string, within time.Duration) {
	if !strings.HasPrefix(dir, s.cfg.DownloadRoot+"/") {
		return
	}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var dirs []string
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() && d.Type()&os.ModeSymlink == 0 {
				dirs = append(dirs, p)
			}
			return nil
		})
		for j := len(dirs) - 1; j >= 0; j-- {
			_ = os.Remove(dirs[j]) // fails on non-empty: fine
		}
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---- content ----

// OpenContent returns a complete file of any torrent, resolved below the
// download root with symlink-safe checks.
func (s *Service) OpenContent(ctx context.Context, id, fid string) (*os.File, os.FileInfo, error) {
	if !s.cfg.Files.ContentAccess {
		return nil, nil, fail(http.StatusNotFound, "content_access_disabled")
	}
	t, err := s.get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	idx, ok := parseFileID(fid)
	if !ok || !t.HasMetadata {
		return nil, nil, fail(http.StatusNotFound, "file_not_found")
	}
	if strings.HasPrefix(t.State, "checking") {
		return nil, nil, fail(http.StatusConflict, "file_incomplete")
	}
	fs, err := s.engine.Files(ctx, id)
	if err != nil {
		return nil, nil, engineErr(err)
	}
	for _, f := range fs {
		if f.Index != idx {
			continue
		}
		if f.Progress < 1 {
			return nil, nil, fail(http.StatusConflict, "file_incomplete")
		}
		fh, info, err := files.Open(s.cfg.DownloadRoot, t.SavePath, f.Name)
		if err != nil {
			if errors.Is(err, files.ErrOutside) || errors.Is(err, files.ErrBadInput) {
				s.log.Warn("content path refused", "task", id, "file", fid)
				return nil, nil, fail(http.StatusForbidden, "path_not_allowed")
			}
			return nil, nil, fail(http.StatusNotFound, "file_not_found")
		}
		if info.Size() != f.Size {
			fh.Close()
			return nil, nil, fail(http.StatusConflict, "file_incomplete")
		}
		return fh, info, nil
	}
	return nil, nil, fail(http.StatusNotFound, "file_not_found")
}
