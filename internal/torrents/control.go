package torrents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/openplayx/bittorrent-server/internal/files"
	"github.com/openplayx/bittorrent-server/internal/storage"
)

func mkdirTask(path string) error { return os.MkdirAll(path, 0o775) }

// ---- reads ----

func (s *Service) GetTask(ctx context.Context, caller, id string) (TaskView, error) {
	r, err := s.loadTask(ctx, id, caller)
	if err != nil {
		return TaskView{}, err
	}
	return s.view(r), nil
}

func (s *Service) ListTasks(ctx context.Context, caller, cursor string, limit int, includeRemoved bool) ([]TaskView, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.store.ListTasks(ctx, caller, includeRemoved, cursor, limit+1)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, "", fail(http.StatusBadRequest, "invalid_cursor")
	}
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[limit-1].ID
	}
	out := make([]TaskView, 0, len(rows))
	for _, row := range rows {
		r, err := decodeTask(row)
		if err != nil {
			return nil, "", err
		}
		out = append(out, s.view(r))
	}
	return out, next, nil
}

type FilesView struct {
	TaskID        string     `json:"task_id"`
	MetadataKnown bool       `json:"metadata_known"`
	FilesRevision int        `json:"files_revision"`
	Files         []FileView `json:"files"`
	Stale         bool       `json:"stale"`
}

func (s *Service) Files(ctx context.Context, caller, id string) (FilesView, error) {
	r, err := s.loadTask(ctx, id, caller)
	if err != nil {
		return FilesView{}, err
	}
	v := FilesView{TaskID: id, FilesRevision: r.body.FilesRevision, Files: []FileView{}}
	if r.snap == nil || !r.snap.HasMetadata {
		return v, nil
	}
	v.MetadataKnown = true
	v.Stale = s.view(r).Stale
	for _, f := range r.snap.Files {
		complete := f.Progress >= 1
		v.Files = append(v.Files, FileView{
			ID: fileID(f.Index), Path: f.Name, Size: f.Size, Progress: f.Progress,
			Selected: f.Priority > 0, Complete: complete, IsVideo: IsVideo(f.Name),
			Available: s.cfg.Files.ContentAccess && complete && r.row.Phase == "active",
		})
	}
	return v, nil
}

func (s *Service) GetOperation(ctx context.Context, caller, id string) (storage.Operation, error) {
	op, err := s.store.GetOperation(ctx, id, caller)
	if errors.Is(err, storage.ErrNotFound) {
		return op, fail(http.StatusNotFound, "operation_not_found")
	}
	return op, err
}

// WaitOperation blocks briefly so fast operations answer synchronously.
func (s *Service) WaitOperation(ctx context.Context, caller, id string, d time.Duration) (storage.Operation, error) {
	deadline := time.Now().Add(d)
	for {
		op, err := s.GetOperation(ctx, caller, id)
		if err != nil || op.Status != "pending" || time.Now().After(deadline) {
			return op, err
		}
		select {
		case <-ctx.Done():
			return op, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ---- mutations ----

// mutation runs fn once per (caller, idempotency key). fn executes in the
// background and returns the operation result; it holds the task lock.
func (s *Service) mutation(ctx context.Context, caller, requestID, kind, taskID, key string, req any,
	precheck func(r *taskRec) error, fn func(ctx context.Context, r *taskRec) (any, error)) (storage.Operation, error) {
	if key == "" {
		key = newID("auto_")
	}
	r, err := s.loadTask(ctx, taskID, caller)
	if err != nil {
		return storage.Operation{}, err
	}
	op, existing, err := s.store.BeginOperation(ctx, storage.Operation{
		ID: newID("op_"), Caller: caller, Kind: kind, TaskID: taskID, IdemKey: key,
		RequestDigest: digest(kind, taskID, req),
	})
	if errors.Is(err, storage.ErrConflict) {
		return storage.Operation{}, fail(http.StatusConflict, "idempotency_key_reused")
	}
	if err != nil || existing {
		return op, err
	}
	if !phaseAllows(kind, r.row.Phase) {
		code := "task_not_active"
		if r.row.Phase == "removed" {
			code = "task_removed"
		}
		_ = s.store.FinishOperation(ctx, op.ID, "failed", code, false, nil)
		return storage.Operation{}, fail(http.StatusConflict, code)
	}
	if precheck != nil {
		if err := precheck(r); err != nil {
			e := engineErr(err)
			_ = s.store.FinishOperation(ctx, op.ID, "failed", e.Code, e.Retryable, nil)
			return storage.Operation{}, e
		}
	}
	s.store.Audit(ctx, caller, requestID, "torrent."+kind, taskID, "")
	s.go_(func(bg context.Context) {
		unlock := s.lock(taskID)
		defer unlock()
		row, err := s.store.GetTaskAny(bg, taskID)
		if err != nil {
			_ = s.store.FinishOperation(bg, op.ID, "failed", "internal_error", true, nil)
			return
		}
		r, err := decodeTask(row)
		if err == nil && !phaseAllows(kind, r.row.Phase) {
			err = fail(http.StatusConflict, "task_not_active")
		}
		var result any
		if err == nil {
			result, err = fn(bg, r)
		}
		if err != nil {
			e := engineErr(err)
			_ = s.store.FinishOperation(bg, op.ID, "failed", e.Code, e.Retryable, map[string]any{"task_id": taskID})
			return
		}
		_ = s.store.FinishOperation(bg, op.ID, "succeeded", "", false, result)
	})
	return op, nil
}

// phaseAllows: a task whose creation failed may still be removed so the
// caller can clean it up; everything else needs an active task.
func phaseAllows(kind, phase string) bool {
	return phase == "active" || (kind == "remove" && phase == "failed")
}

// confirm polls the engine until cond holds for this task's torrent; an
// HTTP 200 from the engine alone proves nothing.
func (s *Service) confirm(ctx context.Context, r *taskRec, cond func(st string, dl, ul int64) bool) (string, error) {
	deadline := time.Now().Add(s.ConfirmTimeout)
	for {
		list, err := s.engine.Info(ctx, r.body.Tag)
		if err == nil {
			found := false
			for _, t := range list {
				if t.Hash == r.body.EngineHash {
					found = true
					if cond(t.State, t.DlLimit, t.UpLimit) {
						return t.State, nil
					}
				}
			}
			if !found {
				return "", fail(http.StatusConflict, "engine_task_missing")
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", &Error{Status: http.StatusGatewayTimeout, Code: "engine_state_unconfirmed", Retryable: true}
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.ConfirmPoll):
		}
	}
}

type ControlRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Service) Pause(ctx context.Context, caller, requestID, id string, req ControlRequest) (storage.Operation, error) {
	return s.mutation(ctx, caller, requestID, "pause", id, req.IdempotencyKey, nil, nil,
		func(ctx context.Context, r *taskRec) (any, error) {
			r.body.UserPaused = true
			if err := s.saveTask(ctx, r, "active"); err != nil {
				return nil, err
			}
			if err := s.engine.Stop(ctx, r.body.EngineHash); err != nil {
				return nil, err
			}
			st, err := s.confirm(ctx, r, func(st string, _, _ int64) bool { return isStopped(st) })
			if err != nil {
				return nil, err
			}
			return map[string]any{"task_id": r.row.ID, "state": mapEngineState(st, true)}, nil
		})
}

// Resume restarts an explicitly identified task. It never force-starts and
// refuses while a quota/disk hold is active.
func (s *Service) Resume(ctx context.Context, caller, requestID, id string, req ControlRequest) (storage.Operation, error) {
	return s.mutation(ctx, caller, requestID, "resume", id, req.IdempotencyKey, nil,
		func(r *taskRec) error {
			switch r.body.HoldReason {
			case "quota_exceeded", "disk_low", "no_files_match_preset":
				return fail(http.StatusConflict, r.body.HoldReason)
			}
			return nil
		},
		func(ctx context.Context, r *taskRec) (any, error) {
			if r.body.HoldReason == "disk_low" || r.body.HoldReason == "quota_exceeded" {
				return nil, fail(http.StatusConflict, r.body.HoldReason)
			}
			r.body.UserPaused, r.body.AutoStart = false, true
			if r.body.HoldReason == "awaiting_selection" {
				r.body.HoldReason = ""
			}
			if r.snap != nil && r.snap.HasMetadata {
				if err := s.fitsQuota(r, selectedRemaining(r.snap.Files)); err != nil {
					return nil, err
				}
			}
			if err := s.saveTask(ctx, r, "active"); err != nil {
				return nil, err
			}
			if err := s.engine.Start(ctx, r.body.EngineHash); err != nil {
				return nil, err
			}
			r.body.Started = true
			_ = s.saveTask(ctx, r, "active")
			st, err := s.confirm(ctx, r, func(st string, _, _ int64) bool { return !isStopped(st) })
			if err != nil {
				return nil, err
			}
			return map[string]any{"task_id": r.row.ID, "state": mapEngineState(st, true)}, nil
		})
}

type LimitsRequest struct {
	DownloadLimit  int64  `json:"download_limit"`
	UploadLimit    int64  `json:"upload_limit"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Service) SetLimits(ctx context.Context, caller, requestID, id string, req LimitsRequest) (storage.Operation, error) {
	dl, okd := clampRate(req.DownloadLimit, s.cfg.Limits.MaxDownloadRate)
	ul, oku := clampRate(req.UploadLimit, s.cfg.Limits.MaxUploadRate)
	if !okd || !oku {
		return storage.Operation{}, fail(http.StatusUnprocessableEntity, "limit_exceeds_maximum")
	}
	return s.mutation(ctx, caller, requestID, "limits", id, req.IdempotencyKey, []int64{req.DownloadLimit, req.UploadLimit}, nil,
		func(ctx context.Context, r *taskRec) (any, error) {
			if err := s.engine.SetLimits(ctx, r.body.EngineHash, dl, ul); err != nil {
				return nil, err
			}
			// The engine reports -1/0 for "unlimited" depending on version.
			norm := func(v int64) int64 {
				if v < 0 {
					return 0
				}
				return v
			}
			if _, err := s.confirm(ctx, r, func(_ string, gotDl, gotUl int64) bool {
				return norm(gotDl) == dl && norm(gotUl) == ul
			}); err != nil {
				return nil, err
			}
			r.body.DownloadLimit, r.body.UploadLimit = dl, ul
			if err := s.saveTask(ctx, r, "active"); err != nil {
				return nil, err
			}
			return map[string]any{"task_id": r.row.ID, "download_limit": dl, "upload_limit": ul}, nil
		})
}

type SelectionRequest struct {
	ExpectedRevision int      `json:"expected_revision"`
	Preset           string   `json:"preset,omitempty"`
	FileIDs          []string `json:"file_ids,omitempty"`
	IdempotencyKey   string   `json:"idempotency_key"`
}

func (s *Service) SetSelection(ctx context.Context, caller, requestID, id string, req SelectionRequest) (storage.Operation, error) {
	if (req.Preset == "") == (req.FileIDs == nil) {
		return storage.Operation{}, fail(http.StatusUnprocessableEntity, "preset_or_file_ids_required")
	}
	if req.Preset != "" && req.Preset != "all" && req.Preset != "video" {
		return storage.Operation{}, fail(http.StatusUnprocessableEntity, "invalid_selection_preset")
	}
	wanted := func(r *taskRec) (map[int]bool, error) {
		if r.snap == nil || !r.snap.HasMetadata {
			return nil, fail(http.StatusConflict, "metadata_pending")
		}
		if r.body.FilesRevision != req.ExpectedRevision {
			return nil, fail(http.StatusConflict, "files_revision_mismatch")
		}
		sel := map[int]bool{}
		known := map[int]bool{}
		for _, f := range r.snap.Files {
			known[f.Index] = true
			if req.Preset == "all" || (req.Preset == "video" && IsVideo(f.Name)) {
				sel[f.Index] = true
			}
		}
		for _, fid := range req.FileIDs {
			idx, ok := parseFileID(fid)
			if !ok || !known[idx] {
				return nil, fail(http.StatusUnprocessableEntity, "unknown_file_id")
			}
			sel[idx] = true
		}
		if len(sel) == 0 {
			return nil, fail(http.StatusUnprocessableEntity, "empty_selection")
		}
		var size int64
		for _, f := range r.snap.Files {
			if sel[f.Index] {
				size += f.Size
			}
		}
		if size > s.maxTaskBytes(r.body.Destination) {
			return nil, fail(http.StatusUnprocessableEntity, "quota_exceeded")
		}
		return sel, nil
	}
	return s.mutation(ctx, caller, requestID, "selection", id, req.IdempotencyKey, req,
		func(r *taskRec) error { _, err := wanted(r); return err },
		func(ctx context.Context, r *taskRec) (any, error) {
			sel, err := wanted(r)
			if err != nil {
				return nil, err
			}
			if err := s.applySelection(ctx, r, sel); err != nil {
				return nil, err
			}
			if r.body.HoldReason == "quota_exceeded" || r.body.HoldReason == "no_files_match_preset" {
				r.body.HoldReason = ""
				if r.body.AutoStart && !r.body.UserPaused {
					if err := s.engine.Start(ctx, r.body.EngineHash); err != nil {
						return nil, err
					}
					r.body.Started = true
				} else {
					r.body.HoldReason = "awaiting_selection"
				}
			}
			if err := s.saveTask(ctx, r, "active"); err != nil {
				return nil, err
			}
			return map[string]any{"task_id": r.row.ID, "files_revision": r.body.FilesRevision, "selected_count": len(sel)}, nil
		})
}

// applySelection writes priorities and verifies them by reading back.
func (s *Service) applySelection(ctx context.Context, r *taskRec, sel map[int]bool) error {
	var on, off []int
	for _, f := range r.snap.Files {
		if sel[f.Index] {
			on = append(on, f.Index)
		} else {
			off = append(off, f.Index)
		}
	}
	if err := s.engine.SetFilePriority(ctx, r.body.EngineHash, off, 0); err != nil {
		return err
	}
	if err := s.engine.SetFilePriority(ctx, r.body.EngineHash, on, 1); err != nil {
		return err
	}
	got, err := s.engine.Files(ctx, r.body.EngineHash)
	if err != nil {
		return err
	}
	for _, f := range got {
		if (f.Priority > 0) != sel[f.Index] {
			return &Error{Status: http.StatusGatewayTimeout, Code: "engine_state_unconfirmed", Retryable: true}
		}
	}
	snap := *r.snap
	snap.Files = toSnapFiles(got)
	_ = s.saveSnapshot(ctx, r, &snap)
	r.body.SelectionDone = true
	r.body.FilesRevision++
	return nil
}

func selectedRemaining(fs []SnapshotFile) int64 {
	var left int64
	for _, f := range fs {
		if f.Priority > 0 {
			left += int64(float64(f.Size) * (1 - f.Progress))
		}
	}
	return left
}

func (s *Service) fitsQuota(r *taskRec, remaining int64) error {
	var selected int64
	for _, f := range r.snap.Files {
		if f.Priority > 0 {
			selected += f.Size
		}
	}
	if selected > s.maxTaskBytes(r.body.Destination) {
		return fail(http.StatusConflict, "quota_exceeded")
	}
	if s.freeBytes != nil {
		if free, err := s.freeBytes(filepath.Dir(r.body.SavePath)); err == nil && remaining+s.cfg.Limits.MinFreeBytes > free {
			return fail(http.StatusConflict, "insufficient_disk_space")
		}
	}
	return nil
}

// ---- removal ----

type RemovePlanRequest struct {
	DeleteFiles bool `json:"delete_files"`
}

type removePlanBody struct {
	DeleteFiles  bool `json:"delete_files"`
	TaskRevision int  `json:"task_revision"`
}

type RemovePlanView struct {
	PlanRef        string    `json:"plan_ref"`
	TaskID         string    `json:"task_id"`
	TaskName       string    `json:"task_name"`
	DeleteFiles    bool      `json:"delete_files"`
	FilesOnDisk    int       `json:"files_on_disk"`
	BytesOnDisk    int64     `json:"bytes_on_disk"`
	SharedWith     []string  `json:"shared_with_tasks"`
	StopsSeeding   bool      `json:"stops_seeding"`
	Effects        []string  `json:"effects"`
	ExpiresAt      time.Time `json:"expires_at"`
	TaskRevision   int       `json:"task_revision"`
	ConfirmationID string    `json:"confirmation_digest"`
}

func (s *Service) RemovePlan(ctx context.Context, caller, id string, req RemovePlanRequest) (RemovePlanView, error) {
	r, err := s.loadTask(ctx, id, caller)
	if err != nil {
		return RemovePlanView{}, err
	}
	if r.row.Phase == "removed" {
		return RemovePlanView{}, fail(http.StatusConflict, "task_removed")
	}
	if r.row.Phase == "creating" {
		return RemovePlanView{}, fail(http.StatusConflict, "task_not_active")
	}
	bytes, count, _ := files.Usage(r.body.SavePath)
	v := RemovePlanView{
		PlanRef: newID("rmp_"), TaskID: id, TaskName: r.body.Name, DeleteFiles: req.DeleteFiles,
		FilesOnDisk: count, BytesOnDisk: bytes, SharedWith: []string{},
		StopsSeeding: r.snap != nil && mapEngineState(r.snap.EngineState, true) == StateSeeding,
		TaskRevision: r.body.Revision, ExpiresAt: time.Now().Add(10 * time.Minute).UTC(),
	}
	// Every task owns a private directory, so no other task can reference
	// these files; the check stays explicit in case layouts change.
	active, _ := s.store.ActiveTasks(ctx)
	for _, t := range active {
		if t.ID == id {
			continue
		}
		if o, err := decodeTask(t); err == nil && o.body.SavePath == r.body.SavePath {
			v.SharedWith = append(v.SharedWith, t.ID)
		}
	}
	if req.DeleteFiles && len(v.SharedWith) > 0 {
		return RemovePlanView{}, fail(http.StatusConflict, "files_shared_with_other_tasks")
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
	v.ConfirmationID = digest(v.PlanRef, id, req.DeleteFiles, r.body.Revision)[:16]
	raw, _ := json.Marshal(removePlanBody{DeleteFiles: req.DeleteFiles, TaskRevision: r.body.Revision})
	if err := s.store.PutRemovePlan(ctx, v.PlanRef, caller, id, raw, 10*time.Minute); err != nil {
		return RemovePlanView{}, err
	}
	return v, nil
}

type RemoveRequest struct {
	PlanRef        string `json:"plan_ref"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Service) Remove(ctx context.Context, caller, requestID, id string, req RemoveRequest) (storage.Operation, error) {
	if req.IdempotencyKey == "" {
		return storage.Operation{}, fail(http.StatusBadRequest, "idempotency_key_required")
	}
	var plan removePlanBody
	check := func(r *taskRec) error {
		taskID, raw, expired, used, err := s.store.GetRemovePlan(ctx, req.PlanRef, caller)
		if errors.Is(err, storage.ErrNotFound) || (err == nil && taskID != id) {
			return fail(http.StatusUnprocessableEntity, "remove_plan_not_found")
		}
		if err != nil {
			return err
		}
		if used {
			return fail(http.StatusConflict, "remove_plan_used")
		}
		if expired {
			return fail(http.StatusConflict, "remove_plan_expired")
		}
		if err := json.Unmarshal(raw, &plan); err != nil {
			return err
		}
		if plan.TaskRevision != r.body.Revision {
			return fail(http.StatusConflict, "task_changed_since_plan")
		}
		return s.store.UseRemovePlan(ctx, req.PlanRef)
	}
	return s.mutation(ctx, caller, requestID, "remove", id, req.IdempotencyKey, req.PlanRef, check,
		func(ctx context.Context, r *taskRec) (any, error) {
			list, err := s.engine.Info(ctx, r.body.Tag)
			if err != nil {
				return nil, err
			}
			present := r.body.EngineHash != "" && slices.ContainsFunc(list, func(t qbTorrent) bool { return t.Hash == r.body.EngineHash })
			if present {
				if err := s.engine.Delete(ctx, r.body.EngineHash); err != nil {
					return nil, err
				}
				if err := s.waitGone(ctx, r); err != nil {
					return nil, err
				}
			}
			if plan.DeleteFiles {
				if err := files.RemoveTaskDir(s.cfg.DownloadRoot, r.body.SavePath); err != nil {
					s.log.Warn("delete files failed", "task", r.row.ID, "err", err)
					r.body.Error = "delete_files_failed"
					r.body.RemovedAt = time.Now().Unix()
					_ = s.saveTask(ctx, r, "removed")
					return nil, fail(http.StatusInternalServerError, "delete_files_failed")
				}
				r.body.DataDeleted = true
			}
			r.body.RemovedAt = time.Now().Unix()
			if err := s.saveTask(ctx, r, "removed"); err != nil {
				return nil, err
			}
			return map[string]any{"task_id": r.row.ID, "data_deleted": plan.DeleteFiles, "files_retained": !plan.DeleteFiles}, nil
		})
}

func (s *Service) waitGone(ctx context.Context, r *taskRec) error {
	deadline := time.Now().Add(s.ConfirmTimeout)
	for {
		list, err := s.engine.Info(ctx, r.body.Tag)
		if err == nil && !slices.ContainsFunc(list, func(t qbTorrent) bool { return t.Hash == r.body.EngineHash }) {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return &Error{Status: http.StatusGatewayTimeout, Code: "engine_state_unconfirmed", Retryable: true}
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.ConfirmPoll):
		}
	}
}

// ---- content ----

// OpenContent returns a complete, authorised file for reading.
func (s *Service) OpenContent(ctx context.Context, caller, id, fid string) (*os.File, os.FileInfo, error) {
	if !s.cfg.Files.ContentAccess {
		return nil, nil, fail(http.StatusNotFound, "content_access_disabled")
	}
	r, err := s.loadTask(ctx, id, caller)
	if err != nil {
		return nil, nil, err
	}
	if r.row.Phase != "active" {
		return nil, nil, fail(http.StatusConflict, "task_not_active")
	}
	idx, ok := parseFileID(fid)
	if !ok || r.snap == nil {
		return nil, nil, fail(http.StatusNotFound, "file_not_found")
	}
	for _, f := range r.snap.Files {
		if f.Index != idx {
			continue
		}
		if f.Progress < 1 || r.snap.EngineState == "checkingUP" || r.snap.EngineState == "checkingDL" {
			return nil, nil, fail(http.StatusConflict, "file_incomplete")
		}
		fh, info, err := files.Open(s.cfg.DownloadRoot, r.body.SavePath, f.Name)
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
