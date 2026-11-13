package torrents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/openplayx/bittorrent-server/internal/qbittorrent"
	"github.com/openplayx/bittorrent-server/internal/storage"
)

// ---- uploaded .torrent inputs ----

type InputView struct {
	InputRef       string    `json:"input_ref"`
	Name           string    `json:"name"`
	TorrentVersion string    `json:"torrent_version"`
	FileCount      int       `json:"file_count"`
	TotalSize      int64     `json:"total_size"`
	Private        bool      `json:"private"`
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
	id := newID("inp_")
	if err := s.store.PutInput(ctx, id, caller, data, ttl); err != nil {
		return InputView{}, err
	}
	return InputView{
		InputRef: id, Name: in.Name, TorrentVersion: in.Identity.Version(), FileCount: len(in.Files),
		TotalSize: in.Size, Private: in.Private, ExpiresAt: time.Now().Add(ttl).UTC(),
	}, nil
}

func inputErr(err error) error {
	var ie *InputError
	if errors.As(err, &ie) {
		return fail(http.StatusUnprocessableEntity, ie.Code)
	}
	return err
}

// ---- plans ----

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

// planBody is persisted; magnet and torrent bytes never leave the server.
type planBody struct {
	Kind            string        `json:"kind"`
	Identity        Identity      `json:"identity"`
	Name            string        `json:"name"`
	Magnet          string        `json:"magnet,omitempty"`
	InputRef        string        `json:"input_ref,omitempty"`
	Destination     string        `json:"destination"`
	DestFingerprint string        `json:"destination_fingerprint"`
	SelectionPreset string        `json:"selection_preset"`
	AutoStart       bool          `json:"auto_start"`
	DownloadLimit   int64         `json:"download_limit"`
	UploadLimit     int64         `json:"upload_limit"`
	Size            int64         `json:"size"`
	Files           []PreviewFile `json:"files,omitempty"`
}

type PlanView struct {
	PlanRef         string          `json:"plan_ref"`
	Revision        int             `json:"revision"`
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
	Warnings        []string        `json:"warnings"`
}

type PlanFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Selected bool   `json:"selected"`
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
		data, gerr := s.store.GetInput(ctx, req.Source.InputRef, caller)
		if errors.Is(gerr, storage.ErrNotFound) {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "input_expired")
		}
		if gerr != nil {
			return PlanView{}, gerr
		}
		in, err = ParseTorrent(ctx, data, s.policy)
	default:
		return PlanView{}, fail(http.StatusUnprocessableEntity, "unsupported_input")
	}
	if err != nil {
		return PlanView{}, inputErr(err)
	}
	autoStart := req.AutoStart == nil || *req.AutoStart
	body := planBody{
		Kind: in.Kind, Identity: in.Identity, Name: in.Name, Magnet: in.Magnet, InputRef: req.Source.InputRef,
		Destination: dest.Ref, DestFingerprint: dest.Fingerprint(s.cfg), SelectionPreset: req.SelectionPreset,
		AutoStart: autoStart, DownloadLimit: dl, UploadLimit: ul, Size: in.Size, Files: in.Files,
	}
	view := PlanView{
		Revision: 1, InputKind: in.Kind, Name: in.Name, TorrentVersion: in.Identity.Version(),
		Metadata: "unknown", SelectionPreset: req.SelectionPreset, AutoStart: autoStart,
		DownloadLimit: dl, UploadLimit: ul, Warnings: []string{},
		Destination: s.destinationView(dest.Ref),
	}
	if in.Kind == "magnet" {
		// No peers are contacted for a preview; discovery starts only once
		// the user creates the task, and that discovery itself is traffic.
		view.Warnings = append(view.Warnings, "metadata_unknown", "metadata_discovery_contacts_peers")
		if in.Identity.V1 == "" {
			view.TorrentVersion = "v2"
		}
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
		if selSize > s.maxTaskBytes(dest.Ref) {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "quota_exceeded")
		}
		if free := view.Destination.FreeBytes; free != nil && selSize+s.cfg.Limits.MinFreeBytes > *free {
			return PlanView{}, fail(http.StatusUnprocessableEntity, "insufficient_disk_space")
		}
	}
	if in.Private {
		view.Warnings = append(view.Warnings, "private_torrent")
	}
	if existing, err := s.store.FindLiveTaskByIdentity(ctx, in.Identity.V1, in.Identity.V2); err == nil {
		e := fail(http.StatusConflict, "duplicate_task")
		if existing.Caller == caller {
			e.Detail = map[string]any{"task_id": existing.ID}
		}
		return PlanView{}, e
	}
	ttl := time.Duration(s.cfg.Limits.PlanTTLSeconds) * time.Second
	raw, _ := json.Marshal(body)
	p := storage.Plan{
		ID: newID("plan_"), Caller: caller, Revision: 1, Body: raw, Torrent: in.Torrent,
		CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(ttl).Unix(),
	}
	if err := s.store.PutPlan(ctx, p); err != nil {
		return PlanView{}, err
	}
	view.PlanRef, view.ExpiresAt = p.ID, time.Unix(p.ExpiresAt, 0).UTC()
	return view, nil
}

// ---- task creation ----

type CreateRequest struct {
	PlanRef          string `json:"plan_ref"`
	ExpectedRevision int    `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key"`
}

func (s *Service) CreateTask(ctx context.Context, caller, requestID string, req CreateRequest) (storage.Operation, error) {
	if req.IdempotencyKey == "" {
		return storage.Operation{}, fail(http.StatusBadRequest, "idempotency_key_required")
	}
	op := storage.Operation{
		ID: newID("op_"), Caller: caller, Kind: "create", IdemKey: req.IdempotencyKey,
		RequestDigest: digest("create", req.PlanRef, req.ExpectedRevision),
	}
	// Idempotent replay is answered before any plan checks: a plan that was
	// consumed by this very key must still return the original operation.
	op, existing, err := s.store.BeginOperation(ctx, op)
	if errors.Is(err, storage.ErrConflict) {
		return storage.Operation{}, fail(http.StatusConflict, "idempotency_key_reused")
	}
	if err != nil || existing {
		return op, err
	}
	failOp := func(e *Error) (storage.Operation, error) {
		_ = s.store.FinishOperation(ctx, op.ID, "failed", e.Code, e.Retryable, nil)
		return storage.Operation{}, e
	}
	p, err := s.store.GetPlan(ctx, req.PlanRef, caller)
	if errors.Is(err, storage.ErrNotFound) {
		return failOp(fail(http.StatusUnprocessableEntity, "plan_not_found"))
	}
	if err != nil {
		return failOp(engineErr(err))
	}
	if p.Revision != req.ExpectedRevision {
		return failOp(fail(http.StatusConflict, "plan_revision_mismatch"))
	}
	if p.UsedBy != "" {
		return failOp(fail(http.StatusConflict, "plan_already_used"))
	}
	if time.Now().Unix() >= p.ExpiresAt {
		return failOp(fail(http.StatusConflict, "plan_expired"))
	}
	var body planBody
	if err := json.Unmarshal(p.Body, &body); err != nil {
		return failOp(engineErr(err))
	}
	dest, ok := s.cfg.Destination(body.Destination)
	if !ok || dest.Fingerprint(s.cfg) != body.DestFingerprint {
		return failOp(fail(http.StatusConflict, "plan_invalidated"))
	}
	active, err := s.store.ActiveTasks(ctx)
	if err != nil {
		return failOp(engineErr(err))
	}
	if len(active) >= s.cfg.Limits.MaxActiveTasks {
		return failOp(fail(http.StatusTooManyRequests, "task_limit_reached"))
	}
	if existing, err := s.store.FindLiveTaskByIdentity(ctx, body.Identity.V1, body.Identity.V2); err == nil {
		e := fail(http.StatusConflict, "duplicate_task")
		if existing.Caller == caller {
			e.Detail = map[string]any{"task_id": existing.ID}
		}
		return failOp(e)
	}
	// Never adopt an engine torrent created outside this service.
	torrents, err := s.engine.Info(ctx, "")
	if err != nil {
		return failOp(engineErr(err))
	}
	for _, t := range torrents {
		if body.Identity.Matches(t.InfohashV1, t.InfohashV2) || body.Identity.Matches(t.Hash, "") {
			return failOp(fail(http.StatusConflict, "engine_conflict"))
		}
	}

	taskID := newID("tsk_")
	if err := s.store.ClaimPlan(ctx, p.ID, taskID); err != nil {
		return failOp(fail(http.StatusConflict, "plan_already_used"))
	}
	tb := TaskBody{
		Name: body.Name, Version: body.Identity.Version(), InputKind: body.Kind, Destination: dest.Ref,
		SavePath: filepath.Join(s.cfg.DestinationPath(dest), taskID), Tag: "opx-" + taskID,
		EngineInstance: s.instance, SelectionPreset: body.SelectionPreset, AutoStart: body.AutoStart,
		DownloadLimit: body.DownloadLimit, UploadLimit: body.UploadLimit, PlanID: p.ID, CreateOp: op.ID,
		Revision: 1,
	}
	raw, _ := json.Marshal(tb)
	if err := s.store.PutTask(ctx, storage.Task{ID: taskID, Caller: caller, Phase: "creating", V1: body.Identity.V1, V2: body.Identity.V2, Body: raw}); err != nil {
		return failOp(engineErr(err))
	}
	_ = s.store.SetOperationTask(ctx, op.ID, taskID)
	op.TaskID = taskID
	s.store.Audit(ctx, caller, requestID, "torrent.create", taskID, body.Kind+" "+body.Identity.Version())
	torrentBytes := p.Torrent
	s.go_(func(bg context.Context) { s.runCreate(bg, op.ID, taskID, body, torrentBytes) })
	return op, nil
}

// runCreate submits the torrent and only reports success once the engine
// lists it. A lost/timeout response is reconciled, never blindly retried.
func (s *Service) runCreate(ctx context.Context, opID, taskID string, body planBody, torrent []byte) {
	unlock := s.lock(taskID)
	defer unlock()
	row, err := s.store.GetTaskAny(ctx, taskID)
	if err != nil {
		return
	}
	r, err := decodeTask(row)
	if err != nil {
		return
	}
	finish := func(status, code string, retryable bool) {
		if status == "succeeded" {
			_ = s.store.FinishOperation(ctx, opID, status, "", false, map[string]any{"task_id": taskID})
			return
		}
		r.body.Error = code
		_ = s.saveTask(ctx, r, "failed")
		_ = s.store.FinishOperation(ctx, opID, status, code, retryable, map[string]any{"task_id": taskID})
	}
	if err := mkdirTask(r.body.SavePath); err != nil {
		finish("failed", "destination_unavailable", true)
		return
	}
	req := qbittorrent.AddRequest{
		SavePath: r.body.SavePath, Tag: ServiceTag + "," + r.body.Tag,
		DownloadLimit: r.body.DownloadLimit, UploadLimit: r.body.UploadLimit,
		RatioLimit: s.cfg.Seeding.RatioLimit, SeedingTime: s.cfg.Seeding.SeedingTimeMinutes,
	}
	if body.Kind == "magnet" {
		// Metadata-first: the engine stops as soon as metadata arrives so
		// the selection is applied before payload transfer starts.
		req.Magnet, req.StopCondition = body.Magnet, "MetadataReceived"
	} else {
		req.Torrent, req.Stopped = torrent, true
	}
	addErr := s.engine.Add(ctx, req)
	if addErr != nil && !qbittorrent.IsCode(addErr, qbittorrent.CodeUnavailable) {
		// Definite rejection: nothing was added.
		finish("failed", engineErr(addErr).Code, false)
		return
	}
	deadline := time.Now().Add(s.ConfirmTimeout)
	for {
		if t, ok := s.findEngine(ctx, body.Identity); ok {
			r.body.EngineHash = t.Hash
			_ = s.saveTask(ctx, r, "active")
			finish("succeeded", "", false)
			s.log.Info("task created", "task", taskID, "kind", body.Kind)
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			code := "creation_unconfirmed"
			if addErr != nil {
				code = "engine_unavailable"
			}
			finish("failed", code, addErr != nil)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.ConfirmPoll):
		}
	}
}

func (s *Service) findEngine(ctx context.Context, id Identity) (qbittorrent.Torrent, bool) {
	list, err := s.engine.Info(ctx, ServiceTag)
	if err != nil {
		return qbittorrent.Torrent{}, false
	}
	for _, t := range list {
		if id.Matches(t.InfohashV1, t.InfohashV2) {
			return t, true
		}
	}
	return qbittorrent.Torrent{}, false
}
