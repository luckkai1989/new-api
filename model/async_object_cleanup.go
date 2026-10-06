package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// AsyncObjectCleanup is written before R2 uploads, so even a crashed admission
// or a failed immediate DELETE leaves a durable cleanup record.
type AsyncObjectCleanup struct {
	ID          int64  `gorm:"primaryKey"`
	KeyHash     string `gorm:"type:varchar(64);uniqueIndex"`
	ObjectKey   string `gorm:"type:text"`
	JobID       string `gorm:"type:varchar(64);index"`
	NextCheckAt int64  `gorm:"index"`
}

func TrackAsyncObject(ctx context.Context, key string) error {
	hash := sha256.Sum256([]byte(key))
	jobID, _, _ := strings.Cut(key, "/")
	entry := AsyncObjectCleanup{KeyHash: hex.EncodeToString(hash[:]), ObjectKey: key, JobID: jobID, NextCheckAt: time.Now().Unix() + 3600}
	return DB.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key_hash"}}, DoNothing: true}).Create(&entry).Error
}

func ForgetAsyncObject(ctx context.Context, key string) error {
	hash := sha256.Sum256([]byte(key))
	return DB.WithContext(ctx).Where("key_hash = ?", hex.EncodeToString(hash[:])).Delete(&AsyncObjectCleanup{}).Error
}
