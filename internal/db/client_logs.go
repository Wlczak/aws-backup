package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ClientLog stores a browser-originated log entry so frontend failures can
// be inspected alongside run history.
type ClientLog struct {
	ID         int64          `gorm:"column:id;primaryKey;autoIncrement"`
	RecordedAt time.Time      `gorm:"column:recorded_at;not null;index"`
	ReceivedAt time.Time      `gorm:"column:received_at;not null;index"`
	Level      string         `gorm:"column:level;not null;index"`
	Source     string         `gorm:"column:source;not null;index"`
	Message    string         `gorm:"column:message;not null"`
	Route      string         `gorm:"column:route;not null;default:''"`
	URL        string         `gorm:"column:url;not null;default:''"`
	Stack      string         `gorm:"column:stack;not null;default:''"`
	SessionID  string         `gorm:"column:session_id;not null;default:''"`
	ContextRaw sql.NullString `gorm:"column:context_json" json:"-"`
	Context    map[string]any `gorm:"-" json:"context,omitempty"`
}

// AfterFind parses the raw JSON context into a friendly map for API
// responses. A malformed payload should not poison the whole list page.
func (c *ClientLog) AfterFind(_ *gorm.DB) error {
	if !c.ContextRaw.Valid || c.ContextRaw.String == "" {
		c.Context = nil
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(c.ContextRaw.String), &v); err != nil {
		c.Context = map[string]any{"raw": c.ContextRaw.String}
		return nil
	}
	c.Context = v
	return nil
}

// ClientLogEntry is the write-side payload used by the API and browser
// logger. RecordedAt is the browser-side timestamp when available.
type ClientLogEntry struct {
	RecordedAt time.Time
	Level      string
	Source     string
	Message    string
	Route      string
	URL        string
	Stack      string
	SessionID  string
	Context    map[string]any
}

// AppendClientLogs stores many browser log entries in one transaction.
func (db *DB) AppendClientLogs(ctx context.Context, entries []ClientLogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	now := time.Now().UTC()
	rows := make([]ClientLog, 0, len(entries))
	for _, e := range entries {
		recorded := e.RecordedAt
		if recorded.IsZero() {
			recorded = now
		} else {
			recorded = recorded.UTC()
		}
		ctxJSON := ""
		if len(e.Context) > 0 {
			b, err := json.Marshal(e.Context)
			if err != nil {
				return fmt.Errorf("marshal client log context: %w", err)
			}
			ctxJSON = string(b)
		}
		rows = append(rows, ClientLog{
			RecordedAt: recorded,
			ReceivedAt: now,
			Level:      e.Level,
			Source:     e.Source,
			Message:    e.Message,
			Route:      e.Route,
			URL:        e.URL,
			Stack:      e.Stack,
			SessionID:  e.SessionID,
			ContextRaw: sql.NullString{String: ctxJSON, Valid: true},
		})
	}
	return db.g.WithContext(ctx).CreateInBatches(rows, sqlChunkSize).Error
}

// ListClientLogs returns a paginated list of browser log entries.
func (db *DB) ListClientLogs(ctx context.Context, page, limit int) ([]ClientLog, int64, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit

	var total int64
	if err := db.g.WithContext(ctx).Model(&ClientLog{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []ClientLog
	err := db.g.WithContext(ctx).
		Order("id DESC").
		Limit(limit).Offset(offset).
		Find(&logs).Error
	return logs, total, err
}

// DeleteClientLogs removes every browser log entry.
func (db *DB) DeleteClientLogs(ctx context.Context) (int64, error) {
	res := db.g.WithContext(ctx).Exec(`DELETE FROM client_logs`)
	return res.RowsAffected, res.Error
}
