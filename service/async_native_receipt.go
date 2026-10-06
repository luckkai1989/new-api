package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const asyncNativeReceiptMaxBytes int64 = 2 << 20

type asyncNativeReceipt struct {
	Version     int                   `json:"version"`
	JobID       string                `json:"job_id"`
	Task        model.Task            `json:"task"`
	PrivateData model.TaskPrivateData `json:"private_data"`
}

// A deterministic credential-free receipt survives a Task INSERT failure after
// the provider returned its ID. It is recovery evidence, never a new POST.
func PersistAsyncNativeTaskReceipt(ctx context.Context, task *model.Task) error {
	if task == nil || task.AsyncJobID == "" {
		return nil
	}
	summary := *task
	summary.ID = 0
	summary.Data = nil
	summary.Properties.Input = ""
	summary.FailReason = model.SanitizeAsyncSummary(summary.FailReason)
	summary.PrivateData.Key = ""
	summary.PrivateData.PluginState = nil
	summary.PrivateData.ResultURL = ""
	summary.AsyncSnapshotHydrated = false
	if summary.PrivateData.BillingContext != nil {
		billing := *summary.PrivateData.BillingContext
		summary.PrivateData.BillingContext = &billing
	}
	raw, err := common.Marshal(asyncNativeReceipt{Version: 1, JobID: task.AsyncJobID, Task: summary, PrivateData: summary.PrivateData})
	if err != nil || int64(len(raw)) > asyncNativeReceiptMaxBytes {
		return errors.New("invalid async native receipt")
	}
	_, err = GetAsyncObjectStore().Put(ctx, task.AsyncJobID+"/native-receipt", "application/json", bytes.NewReader(raw), asyncNativeReceiptMaxBytes)
	if err != nil {
		return errors.New("could not persist async native receipt")
	}
	return nil
}

func RecoverAsyncNativeTaskReceipt(ctx context.Context, job *model.AsyncJob) (*model.Task, error) {
	var existing model.Task
	if err := model.DB.WithContext(ctx).Where("async_job_id = ?", job.JobID).First(&existing).Error; err == nil {
		if existing.UserId != job.UserID {
			return nil, errors.New("native task recovery identity mismatch")
		}
		return &existing, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if job.ResultExpiresAt > 0 && time.Now().Unix() >= job.ResultExpiresAt {
		return nil, errors.New("native recovery receipt expired")
	}
	body, _, _, err := OpenAsyncRecoveryObject(ctx, job.JobID+"/native-receipt", asyncNativeReceiptMaxBytes)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, asyncNativeReceiptMaxBytes+1))
	if err != nil || int64(len(raw)) > asyncNativeReceiptMaxBytes {
		return nil, errors.New("invalid async native receipt")
	}
	var receipt asyncNativeReceipt
	if common.Unmarshal(raw, &receipt) != nil || receipt.Version != 1 || receipt.JobID != job.JobID || receipt.Task.UserId != job.UserID || receipt.Task.ChannelId != job.ChannelID || receipt.Task.TaskID == "" || len(receipt.Task.TaskID) > 191 || strings.ContainsAny(receipt.Task.TaskID, "/\\\x00\r\n") || receipt.Task.Platform == "" || receipt.PrivateData.Key != "" || len(receipt.Task.Data) != 0 && string(receipt.Task.Data) != "null" || len(receipt.PrivateData.PluginState) != 0 || receipt.Task.Properties.Input != "" || receipt.PrivateData.ResultURL != "" {
		return nil, errors.New("native task recovery identity mismatch")
	}
	recovered := receipt.Task
	recovered.Data = nil
	recovered.ID = 0
	recovered.AsyncJobID = job.JobID
	recovered.BusinessMetadata = job.BusinessMetadata
	recovered.PrivateData = receipt.PrivateData
	recovered.PrivateData.TokenId = job.TokenID
	recovered.CreatedAt = time.Now().Unix()
	recovered.UpdatedAt = recovered.CreatedAt
	err = model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked := tx
		if !common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			locked = tx.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var owner model.User
		if err := locked.Where("id = ?", job.UserID).First(&owner).Error; err != nil {
			return err
		}
		var current model.AsyncJob
		if err := locked.Where("job_id = ?", job.JobID).First(&current).Error; err != nil {
			return err
		}
		var collisions []model.Task
		if err := locked.Where("task_id = ? OR async_job_id = ?", recovered.TaskID, job.JobID).Find(&collisions).Error; err != nil {
			return err
		}
		if len(collisions) > 1 {
			return errors.New("native task recovery collision")
		}
		for _, candidate := range collisions {
			if candidate.TaskID != recovered.TaskID || candidate.AsyncJobID != job.JobID || candidate.UserId != job.UserID || candidate.Platform != recovered.Platform {
				return errors.New("native task recovery collision")
			}
			recovered = candidate
			return nil
		}
		return tx.Create(&recovered).Error
	})
	if err != nil {
		return nil, err
	}
	return &recovered, nil
}
