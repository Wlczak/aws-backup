package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Wlczak/aws-backup/internal/db"
)

// Deep Archive pricing the estimator uses. Kept in code (not config) so
// the UI shows the same numbers everyone else sees; real billing still
// comes from AWS.
const (
	pricePerThousandRequests = 0.10  // USD; GET requests for restored objects
	pricePerGBRetrieval      = 0.02  // USD/GB; Deep Archive standard retrieval
	pricePerGBEgress         = 0.09  // USD/GB; internet egress after free tier
	egressFreeGB             = 100.0 // free tier
	retrievalHoursMin        = 12
	retrievalHoursMax        = 48
)

type restoreEstimateRequest struct {
	Paths []string `json:"paths"`
}

type restoreEstimateResponse struct {
	FileCount       int64    `json:"file_count"`
	TotalBytes      int64    `json:"total_bytes"`
	RequestFeeUSD   float64  `json:"request_fee_usd"`
	RetrievalFeeUSD float64  `json:"retrieval_fee_usd"`
	EgressFeeUSD    float64  `json:"egress_fee_usd"`
	TotalFeeUSD     float64  `json:"total_fee_usd"`
	WaitHoursMin    int      `json:"wait_hours_min"`
	WaitHoursMax    int      `json:"wait_hours_max"`
	UnknownPaths    []string `json:"unknown_paths,omitempty"`
}

// handleRestoreEstimate computes a cost breakdown from DB metadata only —
// it does not talk to S3 / AWS.
func (s *Server) handleRestoreEstimate(w http.ResponseWriter, r *http.Request) {
	var req restoreEstimateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %v", errBadJSON, err))
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("paths must be non-empty"))
		return
	}

	want := make(map[string]struct{}, len(req.Paths))
	for _, p := range req.Paths {
		want[p] = struct{}{}
	}

	var (
		count   int64
		bytes   int64
		unknown []string
	)

	// List files in pages; match by path prefix (so a "photos" entry
	// catches "photos/*").
	const pageSize = 1000
	matched := map[string]bool{}
	for p := 1; ; p++ {
		rows, _, err := s.deps.DB.ListFiles(r.Context(), db.FilesFilter{Page: p, Limit: pageSize})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if len(rows) == 0 {
			break
		}
		for _, f := range rows {
			for req := range want {
				if f.Path == req || hasPrefixPath(f.Path, req) {
					count++
					bytes += f.Size
					matched[req] = true
					break
				}
			}
		}
		if len(rows) < pageSize {
			break
		}
	}
	for req := range want {
		if !matched[req] {
			unknown = append(unknown, req)
		}
	}

	gb := float64(bytes) / (1024 * 1024 * 1024)
	request := float64(count) * pricePerThousandRequests / 1000
	retrieval := gb * pricePerGBRetrieval
	egressGB := gb
	if egressGB > egressFreeGB {
		egressGB -= egressFreeGB
	} else {
		egressGB = 0
	}
	egress := egressGB * pricePerGBEgress

	writeJSON(w, http.StatusOK, restoreEstimateResponse{
		FileCount:       count,
		TotalBytes:      bytes,
		RequestFeeUSD:   round2(request),
		RetrievalFeeUSD: round2(retrieval),
		EgressFeeUSD:    round2(egress),
		TotalFeeUSD:     round2(request + retrieval + egress),
		WaitHoursMin:    retrievalHoursMin,
		WaitHoursMax:    retrievalHoursMax,
		UnknownPaths:    unknown,
	})
}

// handleRestoreTrigger is stubbed until feature 19 — actually calling
// s3:RestoreObject is part of the real-AWS-enabled path.
func (s *Server) handleRestoreTrigger(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "restore trigger is gated behind feature 19 (real AWS enablement)",
	})
}

func hasPrefixPath(full, prefix string) bool {
	if len(prefix) >= len(full) {
		return false
	}
	if full[:len(prefix)] != prefix {
		return false
	}
	// Only match on path-component boundary.
	return full[len(prefix)] == '/'
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
