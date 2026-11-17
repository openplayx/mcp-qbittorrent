package torrents

import (
	"context"
	"strconv"
	"time"

	"github.com/openplayx/bittorrent-server/src/internal/qbittorrent"
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

// TaskView is the live projection of one engine torrent. The ID is the
// engine's 40-hex hash. No absolute paths, credentials or magnets.
type TaskView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	State           string     `json:"state"`
	HoldReason      string     `json:"hold_reason,omitempty"`
	ErrorCode       string     `json:"error_code,omitempty"`
	TorrentVersion  string     `json:"torrent_version"`
	Destination     string     `json:"destination_ref,omitempty"`
	AddedVia        string     `json:"added_via"` // "app" (this API) or "other" (WebUI, …)
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
	AddedAt         *time.Time `json:"added_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

func unix(v int64) *time.Time {
	if v <= 0 {
		return nil
	}
	t := time.Unix(v, 0).UTC()
	return &t
}

func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func holdReason(tags map[string]bool) string {
	switch {
	case tags[TagHoldQuota]:
		return "quota_exceeded"
	case tags[TagHoldDisk]:
		return "disk_low"
	case tags[TagHoldNoMatch]:
		return "no_files_match_preset"
	case tags[TagHoldStart]:
		return "awaiting_start"
	}
	return ""
}

func (s *Service) view(t qbittorrent.Torrent) TaskView {
	tags := tagSet(t)
	state := mapEngineState(t.State, t.HasMetadata)
	v := TaskView{
		ID: t.Hash, Name: safeDisplayName(t.Name), State: state, HoldReason: holdReason(tags),
		AddedVia: "other", Progress: t.Progress, SelectedSize: nonNegative(t.Size),
		TotalSize: nonNegative(t.TotalSize), CompletedBytes: nonNegative(t.Completed),
		Downloaded: t.Downloaded, Uploaded: t.Uploaded, DownloadSpeed: t.DlSpeed, UploadSpeed: t.UpSpeed,
		DownloadLimit: nonNegative(t.DlLimit), UploadLimit: nonNegative(t.UpLimit),
		Seeds: t.NumSeeds, Peers: t.NumLeechs, Ratio: t.Ratio, MetadataKnown: t.HasMetadata,
		Seeding: state == StateSeeding, AddedAt: unix(t.AddedOn), CompletedAt: unix(t.CompletionOn),
	}
	if tags[TagService] {
		v.AddedVia = "app"
	}
	switch {
	case t.InfohashV1 != "" && t.InfohashV2 != "":
		v.TorrentVersion = "hybrid"
	case t.InfohashV2 != "":
		v.TorrentVersion = "v2"
	default:
		v.TorrentVersion = "v1"
	}
	if d, ok := s.destinationOf(t.SavePath); ok {
		v.Destination = d.Ref
	}
	switch t.State {
	case "error":
		v.ErrorCode = "engine_error"
	case "missingFiles":
		v.ErrorCode = "files_missing"
	}
	if t.ETA > 0 && t.ETA < 8640000 {
		eta := t.ETA
		v.ETASeconds = &eta
	}
	v.SelectedDone = t.HasMetadata && t.Size > 0 && t.Progress >= 1
	return v
}

// viewLive adds payload completeness, which needs the file list: total_size
// counts v2/hybrid padding, so "selected size == total size" is not a valid
// test. Only torrents whose selection is complete need the extra read.
func (s *Service) viewLive(ctx context.Context, t qbittorrent.Torrent) TaskView {
	v := s.view(t)
	if !v.SelectedDone {
		return v
	}
	fs, err := s.engine.Files(ctx, t.Hash)
	if err != nil || len(fs) == 0 {
		return v
	}
	v.PayloadComplete = true
	for _, f := range fs {
		if f.Progress < 1 {
			v.PayloadComplete = false
			break
		}
	}
	return v
}

// FileView is one file of a torrent, referenced by its engine index.
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

type FilesView struct {
	TaskID        string     `json:"task_id"`
	MetadataKnown bool       `json:"metadata_known"`
	FilesRevision string     `json:"files_revision"`
	Files         []FileView `json:"files"`
}

// filesRevision fingerprints the current selection so a selection change
// can be rejected if someone (App, WebUI) changed it meanwhile.
func filesRevision(fs []qbittorrent.File) string {
	b := make([]byte, 0, len(fs))
	for _, f := range fs {
		if f.Priority > 0 {
			b = append(b, '1')
		} else {
			b = append(b, '0')
		}
	}
	return digest(string(b))[:12]
}

func fileID(index int) string { return "f" + strconv.Itoa(index) }

func parseFileID(id string) (int, bool) {
	if len(id) < 2 || id[0] != 'f' {
		return 0, false
	}
	n, err := strconv.Atoi(id[1:])
	return n, err == nil && n >= 0 && strconv.Itoa(n) == id[1:]
}
