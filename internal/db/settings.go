package db

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Setting is the GORM model for the `settings` table.
type Setting struct {
	Key   string `gorm:"column:key;primaryKey"`
	Value string `gorm:"column:value;not null"`
}

// GetSetting returns (value, ok, err). ok=false means the key is unset.
func (db *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var s Setting
	err := db.g.WithContext(ctx).First(&s, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return s.Value, true, nil
}

// SetSetting upserts a key/value pair.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	return db.g.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value"}),
		}).
		Create(&Setting{Key: key, Value: value}).Error
}
