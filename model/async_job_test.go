package model

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func asyncJobFixture(t *testing.T) *AsyncJob {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.AutoMigrate(&AsyncJob{}, &AsyncJobLedger{}, &AsyncUsageLogOutbox{}, &SubscriptionPreConsumeRecord{}))
	for _, table := range []string{"async_jobs", "async_job_ledgers", "async_usage_log_outboxes", "subscription_pre_consume_records"} {
		require.NoError(t, DB.Exec("DELETE FROM "+table).Error)
		t.Cleanup(func() { DB.Exec("DELETE FROM " + table) })
	}
	require.NoError(t, DB.Create(&User{Id: 9901, Username: "async-account", Quota: 1000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Token{Id: 9902, UserId: 9901, Key: "async-fixture-key", Status: common.TokenStatusEnabled, RemainQuota: 1000}).Error)
	job := &AsyncJob{JobID: "async-fixture", UserID: 9901, TokenID: 9902, IdempotencyScope: "scope-a", IdempotencyKey: "request-1", Fingerprint: "same-body", Status: AsyncJobQueued, NextRunAt: time.Now().Unix(), CreatedAt: time.Now().Unix()}
	_, created, err := CreateAsyncJob(context.Background(), job)
	require.NoError(t, err)
	require.True(t, created)
	return job
}

func TestAsyncJobLedgerAtomicAndIdempotent(t *testing.T) {
	job := asyncJobFixture(t)
	ctx := context.Background()
	_, err := ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(100), 100, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	_, err = ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(100), 100, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	require.NoError(t, SetAsyncSettlementIntent(ctx, job.JobID, 80))
	_, err = ApplyAsyncBilling(ctx, job.JobID, "settle", 80, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	_, err = ApplyAsyncBilling(ctx, job.JobID, "settle", 80, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	_, err = ApplyAsyncBilling(ctx, job.JobID, "settle", 90, 0, "wallet_only", "image-model")
	require.Error(t, err)
	var user User
	var token Token
	require.NoError(t, DB.First(&user, job.UserID).Error)
	require.NoError(t, DB.First(&token, job.TokenID).Error)
	assert.Equal(t, 920, user.Quota)
	assert.Equal(t, 80, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	assert.Equal(t, 920, token.RemainQuota)
	assert.Equal(t, 80, token.UsedQuota)
	var count int64
	require.NoError(t, DB.Model(&AsyncJobLedger{}).Where("job_id = ?", job.JobID).Count(&count).Error)
	assert.EqualValues(t, 2, count)
	require.NoError(t, DB.Model(&AsyncUsageLogOutbox{}).Where("job_id = ?", job.JobID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestAsyncJobLedgerRollbackOnTokenLimit(t *testing.T) {
	job := asyncJobFixture(t)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", job.TokenID).Update("remain_quota", 10).Error)
	_, err := ApplyAsyncBilling(context.Background(), job.JobID, AsyncReservePhase(100), 100, 0, "wallet_only", "image-model")
	require.Error(t, err)
	var user User
	require.NoError(t, DB.First(&user, job.UserID).Error)
	assert.Equal(t, 1000, user.Quota)
	got, err := GetAsyncJob(context.Background(), job.JobID)
	require.NoError(t, err)
	assert.Zero(t, got.BillingReserved)
	var count int64
	require.NoError(t, DB.Model(&AsyncJobLedger{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestAsyncJobRefundDoesNotCountAsSuccessfulConsumption(t *testing.T) {
	job := asyncJobFixture(t)
	ctx := context.Background()
	_, err := ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(100), 100, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = ApplyAsyncBilling(ctx, job.JobID, "refund", 0, 0, "", "image-model")
		require.NoError(t, err)
	}
	var user User
	var token Token
	require.NoError(t, DB.First(&user, job.UserID).Error)
	require.NoError(t, DB.First(&token, job.TokenID).Error)
	assert.Equal(t, 1000, user.Quota)
	assert.Zero(t, user.RequestCount)
	assert.Zero(t, user.UsedQuota)
	assert.Equal(t, 1000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
}

func TestAsyncJobAcceptedGenerationCanSettleAfterAccountDisabled(t *testing.T) {
	job := asyncJobFixture(t)
	ctx := context.Background()
	_, err := ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(100), 100, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", job.UserID).Update("status", common.UserStatusDisabled).Error)
	_, err = ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(120), 120, 0, "wallet_only", "image-model")
	require.Error(t, err, "revocation must still prevent another generation reservation")
	_, err = ApplyAsyncBilling(ctx, job.JobID, "settle", 120, 0, "", "image-model")
	require.NoError(t, err)
	var user User
	require.NoError(t, DB.First(&user, job.UserID).Error)
	assert.Equal(t, 880, user.Quota)
	assert.Equal(t, 120, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
}

func TestAsyncJobZeroPrechargeCanSettleAfterAccountDisabled(t *testing.T) {
	job := asyncJobFixture(t)
	ctx := context.Background()
	_, err := ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(0), 0, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", job.UserID).Update("status", common.UserStatusDisabled).Error)
	_, err = ApplyAsyncBilling(ctx, job.JobID, "settle", 80, 0, "", "image-model")
	require.NoError(t, err)
	var user User
	require.NoError(t, DB.First(&user, job.UserID).Error)
	assert.Equal(t, 920, user.Quota)
	assert.Equal(t, 80, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
}

func TestAsyncJobZeroReservationRetainsFundingPreference(t *testing.T) {
	job := asyncJobFixture(t)
	ctx := context.Background()
	now := time.Now().Unix()
	require.NoError(t, DB.Create(&SubscriptionPlan{Id: 9903, Title: "async", PriceAmount: 1, DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 1000, QuotaResetPeriod: SubscriptionResetNever}).Error)
	require.NoError(t, DB.Create(&UserSubscription{Id: 9904, UserId: job.UserID, PlanId: 9903, AmountTotal: 1000, StartTime: now - 1, EndTime: now + 86400, Status: "active"}).Error)
	_, err := ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(0), 0, 0, "subscription_only", "image-model")
	require.NoError(t, err)
	got, err := ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(100), 100, 0, "subscription_only", "image-model")
	require.NoError(t, err)
	assert.Equal(t, "subscription", got.BillingSource)
	_, err = ApplyAsyncBilling(ctx, job.JobID, "refund", 0, 0, "", "image-model")
	require.NoError(t, err)
	var sub UserSubscription
	require.NoError(t, DB.First(&sub, 9904).Error)
	assert.Zero(t, sub.AmountUsed)
	var user User
	require.NoError(t, DB.First(&user, job.UserID).Error)
	assert.Equal(t, 1000, user.Quota)
}

func TestAsyncJobSubscriptionOverflowRequiresAllActiveConsent(t *testing.T) {
	job := asyncJobFixture(t)
	now := time.Now().Unix()
	require.NoError(t, DB.Create(&SubscriptionPlan{Id: 9903, Title: "async", PriceAmount: 1, DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 1, QuotaResetPeriod: SubscriptionResetNever}).Error)
	for i, allow := range []bool{true, false} {
		require.NoError(t, DB.Create(&UserSubscription{Id: 9904 + i, UserId: job.UserID, PlanId: 9903, AmountTotal: 1, AmountUsed: 1, StartTime: now - 1, EndTime: now + 86400, Status: "active", AllowWalletOverflow: allow}).Error)
	}
	_, err := ApplyAsyncBilling(context.Background(), job.JobID, AsyncReservePhase(100), 100, 0, "subscription_first", "image-model")
	require.Error(t, err)
	var user User
	require.NoError(t, DB.First(&user, job.UserID).Error)
	assert.Equal(t, 1000, user.Quota)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("user_id = ?", job.UserID).Update("allow_wallet_overflow", true).Error)
	got, err := ApplyAsyncBilling(context.Background(), job.JobID, AsyncReservePhase(100), 100, 0, "subscription_first", "image-model")
	require.NoError(t, err)
	assert.Equal(t, "wallet", got.BillingSource)
}

func TestAsyncJobIdempotencyAndExpiredSubmissionNeverRequeued(t *testing.T) {
	job := asyncJobFixture(t)
	ctx := context.Background()
	now := time.Now().Unix()
	copy := *job
	copy.ID = 0
	copy.JobID = "async-other"
	existing, created, err := CreateAsyncJob(ctx, &copy)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, job.JobID, existing.JobID)
	copy.ID = 0
	copy.Fingerprint = "different-body"
	_, _, err = CreateAsyncJob(ctx, &copy)
	require.ErrorIs(t, err, ErrAsyncJobConflict)
	claimed, err := ClaimAsyncJob(ctx, "node-a", now)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	other, err := ClaimAsyncJob(ctx, "node-b", now)
	require.NoError(t, err)
	assert.Nil(t, other)
	require.NoError(t, UpdateAsyncJobLease(ctx, claimed, map[string]any{"status": AsyncJobSubmitting, "lease_until": now - 1}))
	require.NoError(t, MarkExpiredAsyncSubmissions(ctx, now))
	other, err = ClaimAsyncJob(ctx, "node-b", now+1)
	require.NoError(t, err)
	assert.Nil(t, other)
	require.ErrorIs(t, UpdateAsyncJobLease(ctx, claimed, map[string]any{"status": AsyncJobCompleted}), ErrAsyncLeaseLost)
	got, err := GetAsyncJob(ctx, job.JobID)
	require.NoError(t, err)
	assert.Equal(t, AsyncJobUnknown, got.Status)
	assert.Equal(t, now+86400, got.ResultExpiresAt)
}

func TestAsyncJobPendingLimitPreservesIdempotentReplay(t *testing.T) {
	job := asyncJobFixture(t)
	require.NoError(t, SetAsyncPendingPerAccountLimit(1))
	t.Cleanup(func() { _ = SetAsyncPendingPerAccountLimit(100) })
	duplicate := *job
	duplicate.ID = 0
	duplicate.JobID = "duplicate"
	_, created, err := CreateAsyncJob(context.Background(), &duplicate)
	require.NoError(t, err)
	assert.False(t, created)
	next := *job
	next.ID = 0
	next.JobID = "async-next"
	next.IdempotencyKey = "next"
	_, _, err = CreateAsyncJob(context.Background(), &next)
	require.ErrorIs(t, err, ErrAsyncPendingLimit)
	require.NoError(t, DB.Model(job).Update("status", AsyncJobCompleted).Error)
	_, created, err = CreateAsyncJob(context.Background(), &next)
	require.NoError(t, err)
	assert.True(t, created)
}

func TestAsyncJobRefundAfterTokenDeletion(t *testing.T) {
	job := asyncJobFixture(t)
	ctx := context.Background()
	_, err := ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(100), 100, 0, "wallet_only", "image-model")
	require.NoError(t, err)
	require.NoError(t, DB.Delete(&Token{}, job.TokenID).Error)
	_, err = ApplyAsyncBilling(ctx, job.JobID, "refund", 0, 0, "", "image-model")
	require.NoError(t, err)
	var user User
	var token Token
	require.NoError(t, DB.First(&user, job.UserID).Error)
	require.NoError(t, DB.Unscoped().First(&token, job.TokenID).Error)
	assert.Equal(t, 1000, user.Quota)
	assert.Equal(t, 1000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
}
