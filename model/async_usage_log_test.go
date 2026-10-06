package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAsyncUsageOutboxRecoversLogFailureWithoutRebilling(t *testing.T) {
	job := asyncJobFixture(t)
	ctx := context.Background()
	now := time.Now().Unix()
	require.NoError(t, DB.AutoMigrate(&QuotaData{}))
	t.Cleanup(func() { DB.Where("async_job_id = ?", job.JobID).Delete(&QuotaData{}) })
	oldExport := common.DataExportEnabled
	common.DataExportEnabled = true
	t.Cleanup(func() { common.DataExportEnabled = oldExport })
	_, err := ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(100), 100, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	_, err = ApplyAsyncBilling(ctx, job.JobID, "settle", 80, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	require.NoError(t, DB.Model(&AsyncUsageLogOutbox{}).Where("job_id = ?", job.JobID).Update("next_attempt_at", 0).Error)

	// Fail only the independent log-write boundary, not the accounting DB.
	const callback = "async-test-log-outage"
	require.NoError(t, LOG_DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "logs" {
			tx.AddError(errors.New("simulated independent log database outage"))
		}
	}))
	t.Cleanup(func() { _ = LOG_DB.Callback().Create().Remove(callback) })
	require.NoError(t, FlushAsyncUsageLogs(ctx, now))
	var intent AsyncUsageLogOutbox
	require.NoError(t, DB.Where("job_id = ?", job.JobID).First(&intent).Error)
	assert.Zero(t, intent.DeliveredAt)
	assert.Equal(t, now+60, intent.NextAttemptAt)
	var quotaCount int64
	require.NoError(t, DB.Model(&QuotaData{}).Where("async_job_id = ?", job.JobID).Count(&quotaCount).Error)
	assert.Zero(t, quotaCount)

	require.NoError(t, LOG_DB.Callback().Create().Remove(callback))
	require.NoError(t, FlushAsyncUsageLogs(ctx, now+61))
	// Simulate a crash after the log write, before the delivered marker.
	require.NoError(t, DB.Model(&AsyncUsageLogOutbox{}).Where("job_id = ?", job.JobID).
		Updates(map[string]any{"delivered_at": 0, "lease_until": 0, "next_attempt_at": 0}).Error)
	require.NoError(t, FlushAsyncUsageLogs(ctx, now+62))
	var logCount int64
	require.NoError(t, LOG_DB.Model(&Log{}).Where("async_task_id = ?", job.JobID).Count(&logCount).Error)
	assert.EqualValues(t, 1, logCount)
	require.NoError(t, DB.Model(&QuotaData{}).Where("async_job_id = ?", job.JobID).Count(&quotaCount).Error)
	assert.EqualValues(t, 1, quotaCount)
	var user User
	require.NoError(t, DB.First(&user, job.UserID).Error)
	assert.Equal(t, 920, user.Quota)
	assert.Equal(t, 80, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	var ledgers int64
	require.NoError(t, DB.Model(&AsyncJobLedger{}).Where("job_id = ?", job.JobID).Count(&ledgers).Error)
	assert.EqualValues(t, 2, ledgers)
}
