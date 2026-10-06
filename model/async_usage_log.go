package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AsyncUsageLogOutbox is separate from the immutable accounting journal.
// Its payload is compact usage metadata, never a generation body or API key.
type AsyncUsageLogOutbox struct {
	ID            int64  `gorm:"primaryKey"`
	JobID         string `gorm:"type:varchar(64);uniqueIndex"`
	Payload       string `gorm:"type:text"`
	Detailed      bool
	CreatedAt     int64
	NextAttemptAt int64  `gorm:"index"`
	LeaseOwner    string `gorm:"type:varchar(64)"`
	LeaseUntil    int64
	DeliveredAt   int64 `gorm:"index"`
}

func PrepareAsyncUsageLogTx(tx *gorm.DB, job *AsyncJob) error {
	other, err := common.Marshal(map[string]any{"async_task_id": job.JobID, "billing_source": job.BillingSource, "recovered_usage_summary": true})
	if err != nil {
		return err
	}
	log := Log{BusinessMetadata: job.BusinessMetadata, UserId: job.UserID, TokenId: job.TokenID, ChannelId: job.ChannelID, Type: LogTypeConsume,
		CreatedAt: time.Now().Unix(), RequestId: job.JobID, ModelName: job.Model, Quota: job.BillingQuota,
		Content: "Asynchronous generation usage", Other: string(other)}
	log.AsyncTaskID = job.JobID
	payload, err := common.Marshal(log)
	if err != nil {
		return err
	}
	entry := AsyncUsageLogOutbox{JobID: job.JobID, Payload: string(payload), CreatedAt: time.Now().Unix(), NextAttemptAt: time.Now().Unix() + 60}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "job_id"}}, DoNothing: true}).Create(&entry).Error
}

func QueueAsyncUsageLog(ctx context.Context, log *Log) error {
	if log == nil || log.AsyncTaskID == "" {
		return errors.New("async task ID is required for usage delivery")
	}
	var other map[string]any
	_ = common.UnmarshalJsonStr(log.Other, &other)
	safe := map[string]any{"async_task_id": log.AsyncTaskID}
	for _, key := range []string{"model_ratio", "group_ratio", "completion_ratio", "use_price", "model_price", "billing_mode", "expr_b64", "matched_tier", "billing_source", "image_count", "billing_tokens", "cache_tokens", "cache_creation_tokens", "request_rules", "billing_unit", "fixed_price", "quota_saturation"} {
		if value, exists := other[key]; exists {
			safe[key] = value
		}
	}
	if admin, ok := other["admin_info"].(map[string]any); ok {
		if marker, exists := admin["quota_saturation"]; exists {
			safe["admin_info"] = map[string]any{"quota_saturation": marker}
		}
	}
	encoded, err := common.Marshal(safe)
	if err != nil {
		return err
	}
	copy := *log
	copy.Id = 0
	copy.Other = string(encoded)
	copy.Content = "Asynchronous generation usage"
	payload, err := common.Marshal(copy)
	if err != nil {
		return err
	}
	entry := AsyncUsageLogOutbox{JobID: copy.AsyncTaskID, Payload: string(payload), Detailed: true, CreatedAt: time.Now().Unix(), NextAttemptAt: time.Now().Unix()}
	return DB.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "job_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"payload", "detailed", "next_attempt_at"})}).Create(&entry).Error
}

func FlushAsyncUsageLogs(ctx context.Context, now int64) error {
	var entries []AsyncUsageLogOutbox
	if err := DB.WithContext(ctx).Where("delivered_at = 0 AND next_attempt_at <= ? AND lease_until < ?", now, now).Order("id").Limit(40).Find(&entries).Error; err != nil {
		return err
	}
	for _, entry := range entries {
		owner := common.GetRandomString(32)
		claimed := DB.WithContext(ctx).Model(&entry).Where("delivered_at = 0 AND lease_until < ?", now).Updates(map[string]any{"lease_owner": owner, "lease_until": now + 60})
		if claimed.Error != nil {
			return claimed.Error
		}
		if claimed.RowsAffected != 1 {
			continue
		}
		job, err := GetAsyncJob(ctx, entry.JobID)
		if err != nil || job.BillingState != "settled" {
			_ = DB.WithContext(ctx).Model(&entry).Where("lease_owner = ?", owner).Updates(map[string]any{"lease_owner": "", "lease_until": 0, "next_attempt_at": now + 60}).Error
			continue
		}
		var log Log
		if err := common.UnmarshalJsonStr(entry.Payload, &log); err != nil {
			return errors.New("invalid async usage log payload")
		}
		// The ledger is authoritative if a request-local callback carried an
		// earlier reservation or native-task estimation into its log.
		log.Quota = job.BillingQuota
		log.BusinessMetadata = job.BusinessMetadata
		log.AsyncTaskID = job.JobID
		digest := sha256.Sum256([]byte("async-consume:" + entry.JobID))
		eventID := hex.EncodeToString(digest[:])
		log.AsyncLogEventID = &eventID
		writeContext, cancel := context.WithTimeout(ctx, 20*time.Second)
		if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
			var count int64
			err = LOG_DB.WithContext(writeContext).Model(&Log{}).Where("async_log_event_id = ?", eventID).Count(&count).Error
			if err == nil && count == 0 {
				err = LOG_DB.WithContext(writeContext).Create(&log).Error
			}
		} else {
			err = LOG_DB.WithContext(writeContext).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "async_log_event_id"}}, DoNothing: true}).Create(&log).Error
		}
		cancel()
		values := map[string]any{"lease_owner": "", "lease_until": 0}
		if err == nil {
			values["delivered_at"] = now
		} else {
			values["next_attempt_at"] = now + 60
		}
		if updateErr := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err == nil && common.DataExportEnabled {
				if exportErr := InsertAsyncQuotaDataTx(tx, &log, job.JobID); exportErr != nil {
					return exportErr
				}
			}
			return tx.Model(&entry).Where("lease_owner = ?", owner).Updates(values).Error
		}); updateErr != nil {
			return updateErr
		}
	}
	// Preserve only undelivered intents after the public summary retention.
	return DB.WithContext(ctx).Where("delivered_at > 0 AND delivered_at < ?", now-30*24*3600).Delete(&AsyncUsageLogOutbox{}).Error
}

// PendingAsyncUsageJobIDs is a subquery used by summary cleanup, not a Go slice
// that could race a concurrent settlement creating its delivery intent.
func PendingAsyncUsageJobIDs() *gorm.DB {
	return DB.Model(&AsyncUsageLogOutbox{}).Select("job_id").Where("delivered_at = 0")
}
