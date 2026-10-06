package service

import (
	"context"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// AsyncBillingSession delegates all balance changes to a durable SQL journal.
// Refunds are decided by the worker, not by an HTTP failure defer: a transport
// failure does not prove the provider did not start billable work.
type AsyncBillingSession struct {
	JobID   string
	Info    *relaycommon.RelayInfo
	Context *gin.Context
}

func prepareAsyncBilling(c *gin.Context, info *relaycommon.RelayInfo, amount int) *types.NewAPIError {
	id := c.GetString("async_job_id")
	job, err := model.ApplyAsyncBilling(c.Request.Context(), id, model.AsyncReservePhase(amount), amount, info.ChannelId, info.UserSetting.BillingPreference, info.GetBillingModelName())
	if err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	info.Billing = &AsyncBillingSession{JobID: id, Info: info, Context: c}
	info.FinalPreConsumedQuota = job.BillingReserved
	info.BillingSource = job.BillingSource
	info.SubscriptionId = job.BillingSubscriptionID
	return nil
}

func (s *AsyncBillingSession) GetPreConsumedQuota() int {
	job, err := model.GetAsyncJob(context.Background(), s.JobID)
	if err != nil {
		return s.Info.FinalPreConsumedQuota
	}
	return job.BillingReserved
}
func (s *AsyncBillingSession) Reserve(amount int) error {
	job, err := model.ApplyAsyncBilling(s.Context.Request.Context(), s.JobID, model.AsyncReservePhase(amount), amount, s.Info.ChannelId, s.Info.UserSetting.BillingPreference, s.Info.GetBillingModelName())
	if err == nil {
		s.Info.FinalPreConsumedQuota = job.BillingReserved
	}
	return err
}
func (s *AsyncBillingSession) Settle(amount int) error {
	if s.Context.GetBool("async_native_pending") {
		return s.Reserve(amount)
	}
	if err := model.SetAsyncSettlementIntent(s.Context.Request.Context(), s.JobID, amount); err != nil {
		return err
	}
	_, err := model.ApplyAsyncBilling(s.Context.Request.Context(), s.JobID, "settle", amount, s.Info.ChannelId, s.Info.UserSetting.BillingPreference, s.Info.GetBillingModelName())
	return err
}
func (s *AsyncBillingSession) Refund(*gin.Context) {}
func (s *AsyncBillingSession) NeedsRefund() bool   { return false }

func AsyncBillingForTask(ctx context.Context, task *model.Task, quota int, refund bool) error {
	if task.AsyncJobID == "" {
		return fmt.Errorf("task has no async job")
	}
	phase := "settle"
	if refund {
		phase = "refund"
	}
	if !refund {
		if err := model.SetAsyncSettlementIntent(ctx, task.AsyncJobID, quota); err != nil {
			return err
		}
	}
	_, err := model.ApplyAsyncBilling(ctx, task.AsyncJobID, phase, quota, task.ChannelId, "", task.Properties.OriginModelName)
	if err == nil {
		task.Quota = quota
		if refund {
			task.Quota = 0
		}
		err = task.UpdateQuota()
	}
	return err
}

func IsAsyncBilling(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	_, ok := info.Billing.(*AsyncBillingSession)
	return ok
}

// Async job execution opts out of request-local quota caches. SQL is the sole
// authority for reservations even when the legacy request cache is enabled.
func asyncQuotaAuditError(err error) {
	if err != nil {
		common.SysError("async billing reconciliation required: " + err.Error())
	}
}
