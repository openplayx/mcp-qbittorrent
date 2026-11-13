package torrents

import (
	"strconv"
	"time"
)

// Public download states.
const (
	StateMetadataPending = "metadata_pending"
	StateQueued          = "queued"
	StateDownloading     = "downloading"
	StatePaused          = "paused"
	StateChecking        = "checking"
	StateStalled         = "stalled"
	StateCompleted       = "completed"
	StateSeeding         = "seeding"
	StateError           = "error"
	StateRemoved         = "removed"
)

func mapEngineState(engine string, hasMetadata bool) string {
	switch engine {
	case "error", "missingFiles":
		return StateError
	case "metaDL", "forcedMetaDL":
		return StateMetadataPending
	case "stoppedDL", "pausedDL":
		if !hasMetadata {
			return StateMetadataPending
		}
		return StatePaused
	case "queuedDL":
		return StateQueued
	case "downloading", "forcedDL":
		return StateDownloading
	case "stalledDL":
		return StateStalled
	case "checkingDL", "checkingUP", "checkingResumeData", "moving", "allocating":
		return StateChecking
	case "uploading", "forcedUP", "stalledUP":
		return StateSeeding
	case "stoppedUP", "pausedUP", "queuedUP":
		return StateCompleted
	default:
		return StateQueued
	}
}

func isStopped(engine string) bool {
	switch engine {
	case "stoppedDL", "stoppedUP", "pausedDL", "pausedUP":
		return true
	}
	return false
}

// TaskView is the API representation. It never contains engine hashes as
// identifiers, SIDs, full magnets or absolute paths.
type TaskView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	State           string     `json:"state"`
	HoldReason      string     `json:"hold_reason,omitempty"`
	ErrorCode       string     `json:"error_code,omitempty"`
	InputKind       string     `json:"input_kind"`
	TorrentVersion  string     `json:"torrent_version"`
	Destination     string     `json:"destination_ref"`
	Progress        float64    `json:"progress"`
	SelectedSize    int64      `json:"selected_size"`
	TotalSize       int64      `json:"total_size"`
	CompletedBytes  int64      `json:"completed_bytes"`
	Downloaded      int64      `json:"downloaded_bytes"`
	Uploaded        int64      `json:"uploaded_bytes"`
	DownloadSpeed   int64      `json:"download_speed"`
	UploadSpeed     int64      `json:"upload_speed"`
	DownloadLimit   int64      `json:"download_limit"`
	UploadLimit     int64      `json:"upload_limit"`
	Seeds           int        `json:"seeds"`
	Peers           int        `json:"peers"`
	Ratio           float64    `json:"ratio"`
	ETASeconds      *int64     `json:"eta_seconds,omitempty"`
	MetadataKnown   bool       `json:"metadata_known"`
	PayloadComplete bool       `json:"payload_complete"`
	SelectedDone    bool       `json:"selected_files_complete"`
	Seeding         bool       `json:"seeding"`
	FilesRevision   int        `json:"files_revision"`
	Revision        int        `json:"revision"`
	Stale           bool       `json:"stale"`
	EngineStatus    string     `json:"engine_status"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	RemovedAt       *time.Time `json:"removed_at,omitempty"`
	DataDeleted     bool       `json:"data_deleted,omitempty"`
}

func (s *Service) staleAfter() time.Duration {
	return time.Duration(s.cfg.ReconcileIntervalSeconds) * 3 * time.Second
}

func (s *Service) view(r *taskRec) TaskView {
	b := r.body
	v := TaskView{
		ID: r.row.ID, Name: b.Name, HoldReason: b.HoldReason, ErrorCode: b.Error,
		InputKind: b.InputKind, TorrentVersion: b.Version, Destination: b.Destination,
		DownloadLimit: b.DownloadLimit, UploadLimit: b.UploadLimit,
		FilesRevision: b.FilesRevision, Revision: b.Revision,
		CreatedAt: time.Unix(r.row.CreatedAt, 0).UTC(), UpdatedAt: time.Unix(r.row.UpdatedAt, 0).UTC(),
		EngineStatus: "available", DataDeleted: b.DataDeleted,
	}
	if !s.healthy.Load() {
		v.EngineStatus = "unavailable"
	}
	switch r.row.Phase {
	case "removed":
		v.State = StateRemoved
		t := time.Unix(b.RemovedAt, 0).UTC()
		v.RemovedAt = &t
		return v
	case "failed":
		v.State = StateError
		return v
	default:
		// Until the first observation: magnets wait for metadata, files queue.
		v.State = StateQueued
		if b.InputKind == "magnet" {
			v.State = StateMetadataPending
		}
	}
	if snap := r.snap; snap != nil {
		obs := time.Unix(r.row.ObservedAt, 0).UTC()
		v.ObservedAt = &obs
		v.Stale = !s.healthy.Load() || time.Since(obs) > s.staleAfter()
		if snap.Missing {
			v.State, v.ErrorCode = StateError, "engine_task_missing"
		} else if r.row.Phase == "active" {
			v.State = mapEngineState(snap.EngineState, snap.HasMetadata)
		}
		v.MetadataKnown = snap.HasMetadata
		v.Progress = snap.Progress
		v.SelectedSize, v.TotalSize, v.CompletedBytes = snap.Size, snap.TotalSize, snap.Completed
		v.Downloaded, v.Uploaded = snap.Downloaded, snap.Uploaded
		v.DownloadSpeed, v.UploadSpeed = snap.DlSpeed, snap.UpSpeed
		v.Seeds, v.Peers, v.Ratio = snap.Seeds, snap.Peers, snap.Ratio
		if snap.ETA > 0 && snap.ETA < 8640000 {
			eta := snap.ETA
			v.ETASeconds = &eta
		}
		v.Seeding = v.State == StateSeeding
		v.PayloadComplete, v.SelectedDone = completion(snap)
		if v.Stale {
			// A cached "downloading" is not a live fact.
			v.DownloadSpeed, v.UploadSpeed, v.ETASeconds = 0, 0, nil
		}
	} else if r.row.Phase == "active" {
		v.Stale = true
	}
	return v
}

func completion(snap *Snapshot) (payload, selected bool) {
	if !snap.HasMetadata || len(snap.Files) == 0 {
		return false, false
	}
	payload, selected = true, false
	any := false
	allSel := true
	for _, f := range snap.Files {
		if f.Progress < 1 {
			payload = false
		}
		if f.Priority > 0 {
			any = true
			if f.Progress < 1 {
				allSel = false
			}
		}
	}
	return payload, any && allSel
}

// FileView is one file of a task, referenced by a task-scoped file_id.
type FileView struct {
	ID        string  `json:"file_id"`
	Path      string  `json:"path"`
	Size      int64   `json:"size"`
	Progress  float64 `json:"progress"`
	Selected  bool    `json:"selected"`
	Complete  bool    `json:"complete"`
	IsVideo   bool    `json:"is_video"`
	Available bool    `json:"content_available"`
}

func fileID(index int) string { return "f" + strconv.Itoa(index) }

func parseFileID(id string) (int, bool) {
	if len(id) < 2 || id[0] != 'f' {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	return n, err == nil && n >= 0 && strconv.Itoa(n) == id[1:]
}
