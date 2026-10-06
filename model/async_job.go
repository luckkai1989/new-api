package model

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	AsyncJobQueued         = "queued"
	AsyncJobSubmitting     = "submitting"
	AsyncJobPolling        = "polling"
	AsyncJobStoragePending = "storage_pending"
	AsyncJobCompleted      = "completed"
	AsyncJobFailed         = "failed"
	AsyncJobUnknown        = "unknown"
)

type AsyncObjectRef struct {
	Backend     string `json:"backend"`
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// AsyncJob stores execution metadata only. Request and result bytes live in
// the private object store; database rows never contain usable API credentials.
type AsyncJob struct {
	ID      int64  `json:"-" gorm:"primaryKey"`
	JobID   string `json:"id" gorm:"type:varchar(64);uniqueIndex"`
	UserID  int    `json:"-" gorm:"index"`
	TokenID int    `json:"-" gorm:"index"`
	BusinessMetadata
	IdempotencyScope           string `json:"-" gorm:"type:varchar(64);uniqueIndex:idx_async_idempotency,priority:1"`
	IdempotencyKey             string `json:"-" gorm:"type:varchar(128);uniqueIndex:idx_async_idempotency,priority:2"`
	Fingerprint                string `json:"-" gorm:"type:varchar(64)"`
	Endpoint                   string `json:"endpoint" gorm:"type:varchar(191)"`
	Modality                   string `json:"modality" gorm:"type:varchar(16)"`
	Model                      string `json:"model" gorm:"type:varchar(191)"`
	ContentType                string `json:"-" gorm:"type:varchar(191)"`
	ClientIP                   string `json:"-" gorm:"type:varchar(64)"`
	RequestRef                 string `json:"-" gorm:"type:text"`
	ResultRef                  string `json:"-" gorm:"type:text"`
	ArtifactRefs               string `json:"-" gorm:"type:text"`
	Status                     string `json:"status" gorm:"type:varchar(24);index:idx_async_ready,priority:1"`
	ErrorCode                  string `json:"error_code,omitempty" gorm:"type:varchar(64)"`
	ErrorMessage               string `json:"error_message,omitempty" gorm:"type:varchar(512)"`
	NativeTaskID               string `json:"-" gorm:"type:varchar(191);index"`
	LeaseOwner                 string `json:"-" gorm:"type:varchar(96)"`
	LeaseUntil                 int64  `json:"-" gorm:"index"`
	Generation                 int64  `json:"-"`
	NextRunAt                  int64  `json:"-" gorm:"index:idx_async_ready,priority:2"`
	SubmissionStartedAt        int64  `json:"-"`
	CreatedAt                  int64  `json:"created_at" gorm:"index"`
	CompletedAt                int64  `json:"completed_at,omitempty"`
	ResultExpiresAt            int64  `json:"result_expires_at,omitempty" gorm:"index"`
	SummaryExpiresAt           int64  `json:"-" gorm:"index"`
	BillingReserved            int    `json:"-"`
	BillingQuota               int    `json:"quota"`
	BillingState               string `json:"billing_state" gorm:"type:varchar(24)"`
	BillingExpectedQuota       int    `json:"-"`
	BillingSettlementRequested bool   `json:"-"`
	BillingSource              string `json:"-" gorm:"type:varchar(24)"`
	BillingPreference          string `json:"-" gorm:"type:varchar(24)"`
	BillingSubscriptionID      int    `json:"-"`
	ChannelID                  int    `json:"-"`
	ResultHTTPStatus           int    `json:"-"`
}

// Ledger entries and wallet/token mutations commit in the same transaction.
type AsyncJobLedger struct {
	ID        int64  `gorm:"primaryKey"`
	JobID     string `gorm:"type:varchar(64);uniqueIndex:idx_async_ledger_phase,priority:1;index"`
	Phase     string `gorm:"type:varchar(64);uniqueIndex:idx_async_ledger_phase,priority:2"`
	UserID    int
	TokenID   int
	Quota     int
	Delta     int
	CreatedAt int64
}

var ErrAsyncJobConflict = errors.New("idempotency key has a different request")
var ErrAsyncLeaseLost = errors.New("async execution lease lost")
var ErrAsyncPendingLimit = errors.New("account async pending task limit reached")
var asyncPendingLimit atomic.Int64

func SetAsyncPendingPerAccountLimit(limit int) error {
	if limit < 1 || limit > 10000 {
		return errors.New("async pending task limit must be between 1 and 10000")
	}
	asyncPendingLimit.Store(int64(limit))
	return nil
}
func AsyncPendingPerAccountLimit() int64 {
	limit := asyncPendingLimit.Load()
	if limit == 0 {
		return 100
	}
	return limit
}
func AsyncPendingCount(ctx context.Context, userID int) (int64, error) {
	return asyncPendingCountTx(DB.WithContext(ctx), userID)
}
func asyncPendingCountTx(tx *gorm.DB, userID int) (int64, error) {
	var count int64
	err := tx.Model(&AsyncJob{}).Where("user_id = ? AND status IN ?", userID, []string{AsyncJobQueued, AsyncJobSubmitting, AsyncJobPolling, AsyncJobStoragePending}).Count(&count).Error
	return count, err
}
func GetAsyncJobByIdempotency(ctx context.Context, scope, key string) (*AsyncJob, error) {
	var job AsyncJob
	err := DB.WithContext(ctx).Where("idempotency_scope = ? AND idempotency_key = ?", scope, key).First(&job).Error
	return &job, err
}

func CreateAsyncJob(ctx context.Context, job *AsyncJob) (*AsyncJob, bool, error) {
	var persisted AsyncJob
	created := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var owner User
		if err := lockForUpdate(tx).Where("id = ?", job.UserID).First(&owner).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("idempotency_scope = ? AND idempotency_key = ?", job.IdempotencyScope, job.IdempotencyKey).First(&persisted).Error; err == nil {
			if persisted.Fingerprint != job.Fingerprint {
				return ErrAsyncJobConflict
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		count, err := asyncPendingCountTx(tx, job.UserID)
		if err != nil {
			return err
		}
		if count >= AsyncPendingPerAccountLimit() {
			return ErrAsyncPendingLimit
		}
		if err := tx.Create(job).Error; err != nil {
			return err
		}
		persisted = *job
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return &persisted, created, nil
}

func GetAsyncJob(ctx context.Context, id string) (*AsyncJob, error) {
	var job AsyncJob
	err := DB.WithContext(ctx).Where("job_id = ?", id).First(&job).Error
	return &job, err
}

func ClaimAsyncJob(ctx context.Context, owner string, now int64) (*AsyncJob, error) {
	var candidates []AsyncJob
	err := DB.WithContext(ctx).Where("status IN ? AND next_run_at <= ? AND lease_until < ?", []string{AsyncJobQueued, AsyncJobPolling, AsyncJobStoragePending}, now, now).Order("id").Limit(8).Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		result := DB.WithContext(ctx).Model(&AsyncJob{}).Where("id = ? AND generation = ? AND lease_until < ? AND status = ?", candidate.ID, candidate.Generation, now, candidate.Status).
			Updates(map[string]any{"lease_owner": owner, "lease_until": now + 60, "generation": candidate.Generation + 1})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected == 1 {
			candidate.LeaseOwner = owner
			candidate.LeaseUntil = now + 60
			candidate.Generation++
			return &candidate, nil
		}
	}
	return nil, nil
}

func UpdateAsyncJobLease(ctx context.Context, job *AsyncJob, values map[string]any) error {
	result := DB.WithContext(ctx).Model(&AsyncJob{}).Where("id = ? AND lease_owner = ? AND generation = ? AND lease_until >= ?", job.ID, job.LeaseOwner, job.Generation, time.Now().Unix()).Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrAsyncLeaseLost
	}
	return nil
}

func MarkExpiredAsyncSubmissions(ctx context.Context, now int64) error {
	// POSTs already in flight are never returned to the runnable queue.
	return DB.WithContext(ctx).Model(&AsyncJob{}).Where("status = ? AND lease_until < ?", AsyncJobSubmitting, now).
		Updates(map[string]any{"status": AsyncJobUnknown, "error_code": "submission_outcome_unknown", "error_message": "Submission may have reached the provider; it will not be sent again", "completed_at": now, "result_expires_at": now + 24*60*60, "summary_expires_at": now + 30*24*60*60, "lease_owner": "", "lease_until": 0}).Error
}

func ApplyAsyncBilling(ctx context.Context, id, phase string, target, channelID int, preference, modelName string) (*AsyncJob, error) {
	if target < 0 || int64(target) > int64(1<<31-1) {
		return nil, errors.New("invalid async quota")
	}
	var job AsyncJob
	ownerSnapshot, lookupErr := GetAsyncJob(ctx, id)
	if lookupErr != nil {
		return nil, lookupErr
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Admission and billing share account -> job lock ordering, avoiding a
		// queue-cap check racing an idempotent replay against settlement.
		var owner User
		if err := lockForUpdate(tx).Where("id = ?", ownerSnapshot.UserID).First(&owner).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("job_id = ?", id).First(&job).Error; err != nil {
			return err
		}
		var previous AsyncJobLedger
		if err := tx.Where("job_id = ? AND phase = ?", id, phase).First(&previous).Error; err == nil {
			if previous.Quota != target && phase != "refund" {
				return errors.New("async billing replay differs from committed quota")
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if job.BillingState == "settled" || job.BillingState == "refunded" {
			if phase == "settle" || phase == "refund" {
				return nil
			}
			return errors.New("async billing already finalized")
		}
		reserved := job.BillingReserved
		if phase == "refund" {
			target = 0
		}
		if phase != "settle" && phase != "refund" && target < reserved {
			target = reserved
		}
		delta := target - reserved
		if job.BillingPreference == "" && preference != "" {
			job.BillingPreference = common.NormalizeBillingPreference(preference)
		}
		if preference == "" {
			preference = job.BillingPreference
		}
		if job.BillingSource == "" && target > 0 {
			if err := selectAsyncFunding(tx, &job, target, preference, modelName, phase == "settle"); err != nil {
				return err
			}
		} else if job.BillingSource == "subscription" {
			if err := PostConsumeUserSubscriptionDeltaTx(tx, job.BillingSubscriptionID, int64(delta)); err != nil {
				return err
			}
		} else if delta != 0 {
			userUpdate := tx.Model(&User{}).Where("id = ?", job.UserID)
			if delta > 0 {
				userUpdate = userUpdate.Where("quota >= ?", delta)
				if phase != "settle" {
					userUpdate = userUpdate.Where("status = ?", common.UserStatusEnabled)
				}
			}
			result := userUpdate.Update("quota", gorm.Expr("quota - ?", delta))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 && delta != 0 {
				return errors.New("insufficient async wallet quota")
			}
		}
		var token Token
		if err := lockForUpdate(tx).Unscoped().Where("id = ? AND user_id = ?", job.TokenID, job.UserID).First(&token).Error; err != nil {
			return err
		}
		if phase != "settle" && phase != "refund" && delta > 0 && (!token.UnlimitedQuota && token.RemainQuota < delta || token.Status != common.TokenStatusEnabled || token.DeletedAt.Valid || (token.ExpiredTime > 0 && token.ExpiredTime <= time.Now().Unix())) {
			return errors.New("insufficient async token quota")
		}
		if delta != 0 {
			if err := tx.Unscoped().Model(&Token{}).Where("id = ?", token.Id).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", delta), "used_quota": gorm.Expr("used_quota + ?", delta)}).Error; err != nil {
				return err
			}
		}
		if channelID > 0 {
			job.ChannelID = channelID
		}
		job.BillingReserved = target
		if phase == "settle" || phase == "refund" {
			job.BillingQuota = target
			job.BillingState = "settled"
			if phase == "refund" {
				job.BillingState = "refunded"
			}
			if phase == "settle" {
				if err := tx.Model(&User{}).Where("id = ?", job.UserID).Updates(map[string]any{"used_quota": gorm.Expr("used_quota + ?", target), "request_count": gorm.Expr("request_count + ?", 1)}).Error; err != nil {
					return err
				}
				if job.ChannelID > 0 {
					if err := tx.Model(&Channel{}).Where("id = ?", job.ChannelID).Update("used_quota", gorm.Expr("used_quota + ?", target)).Error; err != nil {
						return err
					}
				}
			}
		} else {
			job.BillingState = "reserved"
		}
		if err := tx.Model(&job).Select("billing_source", "billing_preference", "billing_subscription_id", "billing_reserved", "billing_quota", "billing_state", "channel_id").Updates(&job).Error; err != nil {
			return err
		}
		if phase == "settle" {
			if err := PrepareAsyncUsageLogTx(tx, &job); err != nil {
				return err
			}
		}
		return tx.Create(&AsyncJobLedger{JobID: id, Phase: phase, UserID: job.UserID, TokenID: job.TokenID, Quota: target, Delta: delta, CreatedAt: time.Now().Unix()}).Error
	})
	return &job, err
}

func AsyncReservePhase(target int) string { return fmt.Sprintf("reserve:%d", target) }

// Persist the final charge before attempting balance settlement. A process
// death or a transient database failure after a successful relay is recoverable
// without submitting another generation request.
func SetAsyncSettlementIntent(ctx context.Context, id string, quota int) error {
	if quota < 0 || int64(quota) > int64(1<<31-1) {
		return errors.New("invalid async settlement quota")
	}
	return DB.WithContext(ctx).Model(&AsyncJob{}).Where("job_id = ? AND billing_settlement_requested = ?", id, false).Updates(map[string]any{"billing_expected_quota": quota, "billing_settlement_requested": true}).Error
}

func selectAsyncFunding(tx *gorm.DB, job *AsyncJob, amount int, preference, modelName string, finalSettlement bool) error {
	var user User
	query := lockForUpdate(tx).Where("id = ?", job.UserID)
	if !finalSettlement {
		query = query.Where("status = ?", common.UserStatusEnabled)
	}
	if err := query.First(&user).Error; err != nil {
		return err
	}
	tryWallet := func() error {
		result := tx.Model(&User{}).Where("id = ? AND quota >= ?", job.UserID, amount).Update("quota", gorm.Expr("quota - ?", amount))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("insufficient async wallet quota")
		}
		job.BillingSource = "wallet"
		return nil
	}
	trySubscription := func() error {
		reservation, err := PreConsumeUserSubscriptionTx(tx, job.JobID, job.UserID, modelName, 0, int64(amount))
		if err != nil {
			return err
		}
		job.BillingSource = "subscription"
		job.BillingSubscriptionID = reservation.UserSubscriptionId
		return nil
	}
	switch common.NormalizeBillingPreference(preference) {
	case "wallet_only":
		return tryWallet()
	case "subscription_only":
		return trySubscription()
	case "wallet_first":
		if user.Quota >= amount {
			return tryWallet()
		}
		return trySubscription()
	default:
		var subs []UserSubscription
		if err := tx.Where("user_id = ? AND status = ? AND end_time > ?", job.UserID, "active", GetDBTimestampTx(tx)).Find(&subs).Error; err != nil {
			return err
		}
		if len(subs) == 0 {
			return tryWallet()
		}
		if err := trySubscription(); err == nil {
			return nil
		} else {
			for _, sub := range subs {
				if !sub.AllowWalletOverflow {
					return err
				}
			}
			return tryWallet()
		}
	}
}
