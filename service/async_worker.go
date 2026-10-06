package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

const AsyncRelayMaxBytes int64 = 64 << 20

func AsyncTaskUnavailableReason() (string, string) {
	if common.RedisEnabled || common.BatchUpdateEnabled {
		return "unsupported_billing_configuration", "Async tasks require Redis and batch billing to be disabled"
	}
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return "unsupported_log_database", "Async tasks require a relational usage log database for idempotent delivery"
	}
	if !GetAsyncObjectStore().Enabled() {
		return "async_storage_not_configured", "Async tasks require private R2 storage"
	}
	return "", ""
}

var ErrAsyncKeyUnavailable = errors.New("async API key is unavailable before submission")

type asyncExecutionContextKey struct{}

func AsyncJobFromExecutionContext(ctx context.Context) *model.AsyncJob {
	job, _ := ctx.Value(asyncExecutionContextKey{}).(*model.AsyncJob)
	return job
}

var asyncExecutorMu sync.RWMutex
var asyncWorkersWG sync.WaitGroup
var asyncRelayExecutor func(http.ResponseWriter, *http.Request)

func SetAsyncRelayExecutor(executor func(http.ResponseWriter, *http.Request)) {
	asyncExecutorMu.Lock()
	defer asyncExecutorMu.Unlock()
	asyncRelayExecutor = executor
}

type asyncRelayCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
	err    error
}

func (c *asyncRelayCapture) Header() http.Header { return c.header }
func (c *asyncRelayCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}
func (c *asyncRelayCapture) Write(data []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	if int64(c.body.Len()+len(data)) > AsyncRelayMaxBytes {
		c.err = errors.New("async result exceeds 64 MiB")
		return 0, c.err
	}
	return c.body.Write(data)
}
func (c *asyncRelayCapture) Flush() {}

// ExecuteAsyncRelay executes the ordinary authenticated relay route in-process.
// The authorization key is read freshly and never stored with request objects.
func ExecuteAsyncRelay(ctx context.Context, job *model.AsyncJob, path, method string, body io.Reader) (*asyncRelayCapture, error) {
	asyncExecutorMu.RLock()
	executor := asyncRelayExecutor
	asyncExecutorMu.RUnlock()
	if executor == nil {
		return nil, errors.New("async executor not installed")
	}
	token, err := model.GetTokenById(job.TokenID)
	if method == http.MethodGet {
		var original model.Token
		err = model.DB.WithContext(ctx).Unscoped().Where("id = ? AND user_id = ?", job.TokenID, job.UserID).First(&original).Error
		token = &original
	}
	if err != nil || token == nil || token.UserId != job.UserID {
		return nil, ErrAsyncKeyUnavailable
	}
	request, err := http.NewRequestWithContext(context.WithValue(ctx, asyncExecutionContextKey{}, job), method, "http://async.internal"+path, body)
	if err != nil {
		return nil, err
	}
	request.RemoteAddr = net.JoinHostPort(job.ClientIP, "0")
	request.Header.Set("Authorization", "Bearer "+token.Key)
	request.Header.Set("Content-Type", job.ContentType)
	request.Header.Set("X-Business-ID", job.BusinessID)
	request.Header.Set("X-External-User-ID", job.ExternalUserID)
	request.Header.Set("X-External-Task-ID", job.ExternalTaskID)
	capture := &asyncRelayCapture{header: make(http.Header)}
	executor(capture, request)
	if capture.status == 0 {
		capture.status = 200
	}
	return capture, capture.err
}

func StartAsyncTaskWorkers(ctx context.Context, workers int) {
	if code, _ := AsyncTaskUnavailableReason(); code != "" {
		return
	}
	if workers <= 0 {
		workers = 2
	}
	workers = min(workers, 16)
	for range workers {
		owner := common.NodeName + "-" + common.GetRandomString(16)
		asyncWorkersWG.Add(1)
		go func() {
			defer asyncWorkersWG.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				job, err := model.ClaimAsyncJob(ctx, owner, time.Now().Unix())
				if err == nil && job != nil {
					runAsyncJob(ctx, job)
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	asyncWorkersWG.Add(1)
	go func() {
		defer asyncWorkersWG.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = SweepAsyncJobs(ctx, time.Now().Unix())
			}
		}
	}()
}

func WaitAsyncTaskWorkers(ctx context.Context) error {
	done := make(chan struct{})
	go func() { asyncWorkersWG.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runAsyncJob(parent context.Context, job *model.AsyncJob) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if model.UpdateAsyncJobLease(ctx, job, map[string]any{"lease_until": time.Now().Unix() + 60}) != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		if recover() != nil {
			markAsyncTerminal(context.Background(), job, model.AsyncJobUnknown, "worker_panic", "Worker stopped after a submission may have started")
		}
	}()
	if job.Status == model.AsyncJobStoragePending {
		archiveAsyncResult(ctx, job)
		return
	}
	if job.Status == model.AsyncJobPolling {
		pollNativeAsyncJob(ctx, job)
		return
	}
	if time.Now().Unix() >= job.CreatedAt+24*60*60 {
		finalizeUnsubmittedAsyncBilling(ctx, job)
		markAsyncTerminal(ctx, job, model.AsyncJobFailed, "queued_request_expired", "Generation could not start within 24 hours")
		return
	}
	var ref model.AsyncObjectRef
	if common.UnmarshalJsonStr(job.RequestRef, &ref) != nil {
		finalizeUnsubmittedAsyncBilling(ctx, job)
		markAsyncTerminal(ctx, job, model.AsyncJobFailed, "request_unavailable", "Stored request is invalid")
		return
	}
	body, _, _, err := GetAsyncObjectStore().Open(ctx, ref)
	if err != nil {
		_ = model.UpdateAsyncJobLease(ctx, job, map[string]any{"next_run_at": time.Now().Unix() + 30, "lease_until": 0, "lease_owner": ""})
		return
	}
	defer body.Close()
	if err = model.UpdateAsyncJobLease(ctx, job, map[string]any{"status": model.AsyncJobSubmitting, "submission_started_at": time.Now().Unix()}); err != nil {
		return
	}
	job.Status = model.AsyncJobSubmitting
	capture, err := ExecuteAsyncRelay(ctx, job, job.Endpoint, http.MethodPost, body)
	// A Task row is written by the native submit barrier, even when the client
	// protocol subsequently times out. Recover it instead of starting again.
	var native model.Task
	if model.DB.WithContext(ctx).Where("async_job_id = ?", job.JobID).First(&native).Error == nil && (capture == nil || capture.status/100 != 2 || native.Status != model.TaskStatusSuccess) {
		job.NativeTaskID = native.TaskID
		_ = model.UpdateAsyncJobLease(ctx, job, map[string]any{"native_task_id": native.TaskID, "status": model.AsyncJobPolling, "lease_until": 0, "lease_owner": "", "next_run_at": time.Now().Unix()})
		return
	}
	if err != nil {
		if errors.Is(err, ErrAsyncKeyUnavailable) {
			_, refundErr := model.ApplyAsyncBilling(ctx, job.JobID, "refund", 0, 0, "", job.Model)
			asyncQuotaAuditError(refundErr)
			markAsyncTerminal(ctx, job, model.AsyncJobFailed, "api_key_unavailable", "API key was removed before generation started")
			return
		}
		markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "submission_outcome_unknown", "Provider outcome is unknown; this request will not be resent")
		return
	}
	if capture.status/100 != 2 {
		status := model.AsyncJobUnknown
		code := "submission_outcome_unknown"
		message := "Provider outcome is unknown; this request will not be resent"
		if capture.status >= 400 && capture.status < 500 && capture.status != 408 {
			status = model.AsyncJobFailed
			code = "generation_rejected"
			message = "The generation request was rejected"
			_, refundErr := model.ApplyAsyncBilling(ctx, job.JobID, "refund", 0, 0, "", job.Model)
			asyncQuotaAuditError(refundErr)
		}
		markAsyncTerminal(ctx, job, status, code, message)
		return
	}
	job.ResultHTTPStatus = capture.status
	ref, err = putAsyncResult(ctx, job, capture.header.Get("Content-Type"), capture.body.Bytes())
	if err != nil {
		if native.TaskID != "" && native.PrivateData.AsyncSnapshotRef != nil {
			_ = model.UpdateAsyncJobLease(ctx, job, map[string]any{"native_task_id": native.TaskID, "status": model.AsyncJobPolling, "lease_until": 0, "lease_owner": "", "next_run_at": time.Now().Unix() + 30})
			return
		}
		markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "result_archive_unavailable", "Generation finished but its result could not be archived; generation will not be repeated")
		return
	}
	encoded, _ := common.Marshal(ref)
	job.ResultRef = string(encoded)
	now := time.Now().Unix()
	job.CompletedAt = now
	job.ResultExpiresAt = now + 24*60*60
	if model.UpdateAsyncJobLease(ctx, job, map[string]any{"result_ref": job.ResultRef, "status": model.AsyncJobStoragePending, "result_http_status": capture.status, "completed_at": now, "result_expires_at": job.ResultExpiresAt, "summary_expires_at": now + 30*24*60*60}) != nil {
		return
	}
	archiveAsyncResult(ctx, job)
}

// A definitively unsubmitted zero-reservation failure needs a final journal
// marker so its public summary can expire normally. Unknown/in-flight work
// and unreconciled reservations deliberately remain outside this shortcut.
func finalizeUnsubmittedAsyncBilling(ctx context.Context, job *model.AsyncJob) {
	current, err := model.GetAsyncJob(ctx, job.JobID)
	if err != nil || current.SubmissionStartedAt != 0 || current.BillingReserved != 0 || current.BillingState != "" || current.BillingSettlementRequested {
		return
	}
	_, err = model.ApplyAsyncBilling(ctx, job.JobID, "refund", 0, 0, "", job.Model)
	asyncQuotaAuditError(err)
}

func putAsyncResult(ctx context.Context, job *model.AsyncJob, contentType string, raw []byte) (model.AsyncObjectRef, error) {
	var ref model.AsyncObjectRef
	var err error
	for attempt := range 3 {
		ref, err = GetAsyncObjectStore().Put(ctx, job.JobID+"/raw-result", contentType, bytes.NewReader(raw), AsyncRelayMaxBytes)
		if err == nil {
			return ref, nil
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return ref, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return ref, err
}

func markAsyncTerminal(ctx context.Context, job *model.AsyncJob, status, code, message string) {
	now := time.Now().Unix()
	if job.CompletedAt > 0 {
		now = job.CompletedAt
	}
	_ = model.UpdateAsyncJobLease(ctx, job, map[string]any{"status": status, "error_code": code, "error_message": message, "completed_at": now, "result_expires_at": now + 24*60*60, "summary_expires_at": now + 30*24*60*60, "lease_owner": "", "lease_until": 0})
}

func archiveAsyncResult(ctx context.Context, job *model.AsyncJob) {
	if job.ResultExpiresAt > 0 && time.Now().Unix() >= job.ResultExpiresAt {
		markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "result_archive_expired", "Result archival did not finish within the 24 hour retention window")
		return
	}
	current, err := model.GetAsyncJob(ctx, job.JobID)
	if err != nil {
		return
	}
	if current.BillingState != "settled" {
		if !current.BillingSettlementRequested {
			markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "billing_reconciliation_required", "Generation completed but its charge requires reconciliation")
			return
		}
		if _, err := model.ApplyAsyncBilling(ctx, job.JobID, "settle", current.BillingExpectedQuota, current.ChannelID, "", job.Model); err != nil {
			releaseAsyncArchive(ctx, job)
			return
		}
	}
	store := GetAsyncObjectStore()
	var ref model.AsyncObjectRef
	if common.UnmarshalJsonStr(job.ResultRef, &ref) != nil {
		markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "result_archive_unavailable", "Saved result reference is invalid")
		return
	}
	body, contentType, _, err := store.Open(ctx, ref)
	if err != nil {
		releaseAsyncArchive(ctx, job)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(body, AsyncRelayMaxBytes+1))
	body.Close()
	if err != nil || int64(len(raw)) > AsyncRelayMaxBytes {
		markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "result_archive_invalid", "Result could not be read")
		return
	}
	artifacts := map[string]model.AsyncObjectRef{}
	if strings.Contains(contentType, "json") {
		var result map[string]any
		if common.Unmarshal(raw, &result) == nil && result != nil {
			if entry, ok := result["data"].(map[string]any); ok {
				result["data"] = []any{entry}
			}
			if data, ok := result["data"].([]any); ok {
				for index, item := range data {
					entry, ok := item.(map[string]any)
					if !ok {
						continue
					}
					encoded, _ := entry["b64_json"].(string)
					url, _ := entry["url"].(string)
					if encoded == "" && url == "" {
						continue
					}
					var reader io.Reader
					var closer io.Closer
					mimeType := "image/png"
					if encoded != "" {
						reader = base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
					} else {
						response, downloadErr := DownloadAsyncArtifact(ctx, url)
						if downloadErr != nil || response == nil {
							releaseAsyncArchive(ctx, job)
							return
						}
						if response.StatusCode/100 != 2 {
							response.Body.Close()
							releaseAsyncArchive(ctx, job)
							return
						}
						reader = response.Body
						closer = response.Body
						mimeType = response.Header.Get("Content-Type")
					}
					key := fmt.Sprintf("image-%d", index+1)
					artifact, putErr := store.Put(ctx, job.JobID+"/artifacts/"+key, mimeType, reader, AsyncRelayMaxBytes)
					if closer != nil {
						closer.Close()
					}
					if putErr != nil {
						releaseAsyncArchive(ctx, job)
						return
					}
					artifacts[key] = artifact
					entry["url"] = "/v1/async/tasks/" + job.JobID + "/artifacts/" + key
					delete(entry, "b64_json")
				}
			}
			if encoded, encodeErr := common.Marshal(result); encodeErr == nil {
				raw = encoded
			}
		} else {
			markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "result_archive_invalid", "Generated result was not valid JSON")
			return
		}
		if job.Modality == "image" && len(artifacts) == 0 {
			markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "result_archive_unavailable", "Image generation returned no retrievable image")
			return
		}
	} else {
		// Keep binary/text bytes behind the artifact endpoint. /result is
		// always structured JSON and must never replace the binary reference.
		artifact, putErr := store.Put(ctx, job.JobID+"/artifacts/output", contentType, bytes.NewReader(raw), AsyncRelayMaxBytes)
		if putErr != nil {
			releaseAsyncArchive(ctx, job)
			return
		}
		artifacts["output"] = artifact
		artifactType := job.Modality
		if strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "subrip") {
			artifactType = "text"
		}
		raw, err = common.Marshal(map[string]any{"id": job.JobID, "model": job.Model, "status": "completed", "data": []any{map[string]any{"id": "output", "type": artifactType, "mime_type": artifact.ContentType, "size": artifact.Size, "url": "/v1/async/tasks/" + job.JobID + "/artifacts/output"}}})
		if err != nil {
			releaseAsyncArchive(ctx, job)
			return
		}
		contentType = "application/json"
	}
	normalized, err := store.Put(ctx, job.JobID+"/result", contentType, bytes.NewReader(raw), AsyncRelayMaxBytes)
	if err != nil {
		releaseAsyncArchive(ctx, job)
		return
	}
	encoded, _ := common.Marshal(normalized)
	artifactBytes, _ := common.Marshal(artifacts)
	now := time.Now().Unix()
	completed := job.CompletedAt
	if completed == 0 {
		completed = now
	}
	if model.UpdateAsyncJobLease(ctx, job, map[string]any{"result_ref": string(encoded), "artifact_refs": string(artifactBytes), "status": model.AsyncJobCompleted, "completed_at": completed, "result_expires_at": completed + 24*60*60, "summary_expires_at": completed + 30*24*60*60, "lease_until": 0, "lease_owner": ""}) == nil {
		_ = store.Delete(ctx, ref)
		clearAsyncNativePayload(ctx, job.JobID)
	}
}

func releaseAsyncArchive(ctx context.Context, job *model.AsyncJob) {
	_ = model.UpdateAsyncJobLease(ctx, job, map[string]any{"status": model.AsyncJobStoragePending, "next_run_at": time.Now().Unix() + 30, "lease_until": 0, "lease_owner": ""})
}

func pollNativeAsyncJob(ctx context.Context, job *model.AsyncJob) {
	var task model.Task
	if model.DB.WithContext(ctx).Where("task_id = ? AND async_job_id = ?", job.NativeTaskID, job.JobID).First(&task).Error != nil {
		markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "native_task_unavailable", "Native task could not be recovered")
		return
	}
	if task.Status != model.TaskStatusSuccess && task.Status != model.TaskStatusFailure {
		_ = model.UpdateAsyncJobLease(ctx, job, map[string]any{"next_run_at": time.Now().Unix() + 10, "lease_until": 0, "lease_owner": ""})
		return
	}
	if task.Status == model.TaskStatusFailure {
		err := AsyncBillingForTask(ctx, &task, 0, true)
		if err != nil {
			asyncQuotaAuditError(err)
			return
		}
		markAsyncTerminal(ctx, job, model.AsyncJobFailed, "generation_failed", "Native generation failed")
		return
	}
	if task.FinishTime > 0 && time.Now().Unix() >= task.FinishTime+24*60*60 {
		job.CompletedAt = task.FinishTime
		markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "result_expired", "Native task result expired before archival")
		return
	}
	if err := HydrateAsyncNativeTask(ctx, &task); err != nil {
		_ = model.UpdateAsyncJobLease(ctx, job, map[string]any{"next_run_at": time.Now().Unix() + 30, "lease_until": 0, "lease_owner": ""})
		return
	}
	if err := reconcileAsyncNativeBilling(ctx, &task); err != nil {
		asyncQuotaAuditError(err)
		return
	}
	listing, err := ExecuteAsyncRelay(ctx, job, "/v1/tasks/"+task.TaskID+"/artifacts", http.MethodGet, nil)
	if err != nil || listing.status != 200 {
		_ = model.UpdateAsyncJobLease(ctx, job, map[string]any{"next_run_at": time.Now().Unix() + 30, "lease_until": 0, "lease_owner": ""})
		return
	}
	var envelope struct {
		Artifacts []struct {
			Key      string `json:"key"`
			Type     string `json:"type"`
			MimeType string `json:"mime_type"`
		} `json:"artifacts"`
		LegacyContentURL string `json:"legacy_content_url"`
	}
	if common.Unmarshal(listing.body.Bytes(), &envelope) != nil {
		return
	}
	refs := map[string]model.AsyncObjectRef{}
	outputs := []map[string]any{}
	if len(envelope.Artifacts) == 0 && envelope.LegacyContentURL != "" {
		content, err := ExecuteAsyncRelay(ctx, job, "/v1/videos/"+task.TaskID+"/content", http.MethodGet, nil)
		if err != nil || content.status != 200 {
			return
		}
		ref, err := GetAsyncObjectStore().Put(ctx, job.JobID+"/artifacts/output", content.header.Get("Content-Type"), bytes.NewReader(content.body.Bytes()), AsyncRelayMaxBytes)
		if err != nil {
			return
		}
		refs["output"] = ref
		outputs = append(outputs, map[string]any{"id": "output", "type": job.Modality, "url": "/v1/async/tasks/" + job.JobID + "/artifacts/output"})
	}
	for _, artifact := range envelope.Artifacts {
		content, err := ExecuteAsyncRelay(ctx, job, "/v1/tasks/"+task.TaskID+"/artifacts/"+artifact.Key+"/content", http.MethodGet, nil)
		if err != nil || content.status != 200 {
			return
		}
		ref, err := GetAsyncObjectStore().Put(ctx, job.JobID+"/artifacts/"+artifact.Key, content.header.Get("Content-Type"), bytes.NewReader(content.body.Bytes()), AsyncRelayMaxBytes)
		if err != nil {
			return
		}
		refs[artifact.Key] = ref
		outputs = append(outputs, map[string]any{"id": artifact.Key, "type": artifact.Type, "url": "/v1/async/tasks/" + job.JobID + "/artifacts/" + artifact.Key})
	}
	if len(outputs) == 0 {
		markAsyncTerminal(ctx, job, model.AsyncJobUnknown, "result_archive_unavailable", "Native generation completed but no retrievable artifact was found")
		return
	}
	raw, _ := common.Marshal(map[string]any{"id": job.JobID, "model": job.Model, "status": "completed", "data": outputs})
	ref, err := GetAsyncObjectStore().Put(ctx, job.JobID+"/result", "application/json", bytes.NewReader(raw), AsyncRelayMaxBytes)
	if err != nil {
		return
	}
	resultBytes, _ := common.Marshal(ref)
	artifactBytes, _ := common.Marshal(refs)
	now := time.Now().Unix()
	completed := task.FinishTime
	if completed == 0 {
		completed = now
	}
	if model.UpdateAsyncJobLease(ctx, job, map[string]any{"result_ref": string(resultBytes), "artifact_refs": string(artifactBytes), "status": model.AsyncJobCompleted, "completed_at": completed, "result_expires_at": completed + 24*60*60, "summary_expires_at": completed + 30*24*60*60, "lease_until": 0, "lease_owner": ""}) == nil {
		clearAsyncNativePayload(ctx, job.JobID)
	}
}

func reconcileAsyncNativeBilling(ctx context.Context, task *model.Task) error {
	if err := HydrateAsyncNativeTask(ctx, task); err != nil {
		return err
	}
	job, err := model.GetAsyncJob(ctx, task.AsyncJobID)
	if err != nil {
		return err
	}
	if job.BillingState == "settled" || job.BillingState == "refunded" {
		return nil
	}
	if job.BillingSettlementRequested {
		_, err = model.ApplyAsyncBilling(ctx, job.JobID, "settle", job.BillingExpectedQuota, task.ChannelId, "", task.Properties.OriginModelName)
		return err
	}
	if GetTaskAdaptorFunc != nil && len(task.Data) > 0 {
		adaptor := GetTaskAdaptorFunc(constant.TaskPlatform(task.Platform))
		if adaptor != nil {
			channel, err := model.GetChannelById(task.ChannelId, true)
			if err != nil {
				return err
			}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: channel.Type, ChannelId: channel.Id, ChannelBaseUrl: channel.GetBaseURL()}}
			info.ApiKey = channel.Key
			adaptor.Init(info)
			result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: 200, Header: make(http.Header)}, task.Data)
			if err != nil {
				return err
			}
			settleTaskBillingOnComplete(ctx, adaptor, task, result)
		}
	}
	job, err = model.GetAsyncJob(ctx, task.AsyncJobID)
	if err != nil {
		return err
	}
	if job.BillingSettlementRequested {
		_, err = model.ApplyAsyncBilling(ctx, job.JobID, "settle", job.BillingExpectedQuota, task.ChannelId, "", task.Properties.OriginModelName)
		return err
	}
	return AsyncBillingForTask(ctx, task, task.Quota, false)
}

func clearAsyncNativePayload(ctx context.Context, id string) {
	var task model.Task
	if model.DB.WithContext(ctx).Where("async_job_id = ?", id).First(&task).Error == nil {
		if task.PrivateData.AsyncSnapshotRef != nil {
			_ = GetAsyncObjectStore().Delete(ctx, *task.PrivateData.AsyncSnapshotRef)
			task.PrivateData.AsyncSnapshotRef = nil
		}
		task.Data = nil
		task.PrivateData.PluginState = nil
		task.PrivateData.ResultURL = ""
		task.FailReason = model.SanitizeAsyncSummary(task.FailReason)
		task.Properties.Input = ""
		_ = task.Update()
	}
}

func SweepAsyncJobs(ctx context.Context, now int64) error {
	if err := model.MarkExpiredAsyncSubmissions(ctx, now); err != nil {
		return err
	}
	var unknown []model.AsyncJob
	if err := model.DB.WithContext(ctx).Where("status = ? AND error_code IN ? AND next_run_at <= ?", model.AsyncJobUnknown, []string{"submission_outcome_unknown", "worker_panic"}, now).Order("next_run_at, id").Limit(20).Find(&unknown).Error; err != nil {
		return err
	}
	for _, job := range unknown {
		if task, err := RecoverAsyncNativeTaskReceipt(ctx, &job); err == nil {
			_ = model.DB.WithContext(ctx).Model(&model.AsyncJob{}).Where("id = ? AND status = ?", job.ID, model.AsyncJobUnknown).Updates(map[string]any{"status": model.AsyncJobPolling, "native_task_id": task.TaskID, "next_run_at": now, "error_code": "", "error_message": ""}).Error
		} else {
			// Fairly rotate missing/temporarily unavailable receipts. An early
			// batch of genuinely unknown synchronous jobs must not starve a
			// later recoverable native provider ID forever.
			_ = model.DB.WithContext(ctx).Model(&model.AsyncJob{}).Where("id = ? AND status = ?", job.ID, model.AsyncJobUnknown).Update("next_run_at", now+60).Error
		}
	}
	var expired []model.AsyncJob
	if err := model.DB.WithContext(ctx).Where("result_expires_at > 0 AND result_expires_at <= ?", now).Where("request_ref <> ? OR result_ref <> ? OR artifact_refs <> ?", "", "", "").Limit(20).Find(&expired).Error; err != nil {
		return err
	}
	for _, job := range expired {
		refs := map[string]model.AsyncObjectRef{}
		_ = common.UnmarshalJsonStr(job.ArtifactRefs, &refs)
		for _, field := range []string{job.RequestRef, job.ResultRef} {
			var ref model.AsyncObjectRef
			if common.UnmarshalJsonStr(field, &ref) == nil && ref.Key != "" {
				refs[ref.Key] = ref
			}
		}
		allDeleted := true
		for _, ref := range refs {
			if GetAsyncObjectStore().Delete(ctx, ref) != nil {
				allDeleted = false
			}
		}
		clearAsyncNativePayload(ctx, job.JobID)
		if allDeleted {
			_ = model.DB.WithContext(ctx).Model(&job).Updates(map[string]any{"request_ref": "", "result_ref": "", "artifact_refs": ""}).Error
		}
	}
	// Unreconciled reservations remain available to administrators and the
	// ledger; the 30-day public summary TTL does not silently forgive charges.
	if err := model.DB.WithContext(ctx).Where("summary_expires_at < ? AND billing_state IN ?", now, []string{"settled", "refunded"}).Where("request_ref = ? AND result_ref = ? AND artifact_refs = ?", "", "", "").Where("job_id NOT IN (?)", model.PendingAsyncUsageJobIDs()).Delete(&model.AsyncJob{}).Error; err != nil {
		return err
	}
	if err := model.FlushAsyncUsageLogs(ctx, now); err != nil {
		return err
	}
	return SweepAsyncTrackedObjects(ctx, now)
}
