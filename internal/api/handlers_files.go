package api

import (
	"net/http"
	"time"

	"github.com/Wlczak/aws-backup/internal/db"
)

type filesListResponse struct {
	Files []fileEntry `json:"files"`
	Total int64       `json:"total"`
	Page  int         `json:"page"`
	Limit int         `json:"limit"`
}

type fileEntry struct {
	ID         int64     `json:"id"`
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	MTime      time.Time `json:"mtime"`
	MD5        string    `json:"md5,omitempty"`
	Status     string    `json:"status"`
	ZipName    string    `json:"zip_name,omitempty"`
	S3Key      string    `json:"s3_key,omitempty"`
	UploadedAt time.Time `json:"uploaded_at,omitempty"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	files, total, err := s.deps.DB.ListFiles(r.Context(), db.FilesFilter{
		Status: q.Get("status"),
		Search: q.Get("search"),
		Page:   intParam(r, "page", 1),
		Limit:  intParam(r, "limit", 50),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]fileEntry, 0, len(files))
	for _, f := range files {
		out = append(out, fileEntry{
			ID: f.ID, Path: f.Path, Size: f.Size, MTime: f.MTime, MD5: f.MD5,
			Status: f.Status, ZipName: f.ZipName, S3Key: f.S3Key,
			UploadedAt: f.UploadedAt, LastSeenAt: f.LastSeenAt,
		})
	}
	writeJSON(w, http.StatusOK, filesListResponse{
		Files: out, Total: total,
		Page:  intParam(r, "page", 1),
		Limit: intParam(r, "limit", 50),
	})
}

type fileStatsResponse struct {
	ByStatus   map[string]int64 `json:"by_status"`
	TotalCount int64            `json:"total_count"`
	TotalSize  int64            `json:"total_size"`
}

func (s *Server) handleFileStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.deps.DB.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, fileStatsResponse{
		ByStatus: st.ByStatus, TotalCount: st.TotalCount, TotalSize: st.TotalSize,
	})
}
