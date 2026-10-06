package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type asyncMemoryStore struct {
	mu           sync.Mutex
	objects      map[string][]byte
	mime         map[string]string
	failArtifact int
}

func (s *asyncMemoryStore) Enabled() bool { return true }
func (s *asyncMemoryStore) Put(_ context.Context, key, contentType string, reader io.Reader, max int64) (AsyncObjectRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.Contains(key, "/artifacts/") && s.failArtifact > 0 {
		s.failArtifact--
		return AsyncObjectRef{}, errors.New("temporary storage outage")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return AsyncObjectRef{}, err
	}
	if int64(len(raw)) > max {
		return AsyncObjectRef{}, errors.New("too large")
	}
	s.objects[key] = raw
	s.mime[key] = contentType
	return AsyncObjectRef{Backend: "r2", Key: key, ContentType: contentType, Size: int64(len(raw))}, nil
}
func (s *asyncMemoryStore) Open(_ context.Context, ref AsyncObjectRef) (io.ReadCloser, string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.objects[ref.Key]
	if !ok {
		return nil, "", 0, errors.New("missing object")
	}
	if ref.Size != int64(len(raw)) {
		return nil, "", 0, errors.New("object reference size mismatch")
	}
	return io.NopCloser(bytes.NewReader(raw)), s.mime[ref.Key], int64(len(raw)), nil
}
func (s *asyncMemoryStore) Lookup(_ context.Context, key string, maxBytes int64) (AsyncObjectRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.objects[key]
	if !ok || int64(len(raw)) > maxBytes {
		return AsyncObjectRef{}, errors.New("missing or oversized object")
	}
	return AsyncObjectRef{Backend: "r2", Key: key, ContentType: s.mime[key], Size: int64(len(raw))}, nil
}
func (s *asyncMemoryStore) Delete(_ context.Context, ref AsyncObjectRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, ref.Key)
	delete(s.mime, ref.Key)
	return nil
}

func asyncWorkerFixture(t *testing.T) (*model.AsyncJob, *asyncMemoryStore) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.AsyncJob{}, &model.AsyncJobLedger{}, &model.AsyncUsageLogOutbox{}, &model.AsyncObjectCleanup{}, &model.SubscriptionPreConsumeRecord{}))
	for _, table := range []string{"async_jobs", "async_job_ledgers", "async_usage_log_outboxes", "async_object_cleanups"} {
		require.NoError(t, model.DB.Exec("DELETE FROM "+table).Error)
		t.Cleanup(func() { model.DB.Exec("DELETE FROM " + table) })
	}
	user := &model.User{Id: 9951, Username: "async-worker", AffCode: "async-worker", Quota: 1000, Status: common.UserStatusEnabled}
	token := &model.Token{Id: 9952, UserId: 9951, Key: "async-worker-fixture", Status: common.TokenStatusEnabled, RemainQuota: 1000}
	require.NoError(t, model.DB.Create(user).Error)
	require.NoError(t, model.DB.Create(token).Error)
	t.Cleanup(func() {
		model.DB.Unscoped().Delete(&model.Token{}, 9952)
		model.DB.Unscoped().Delete(&model.User{}, 9951)
	})
	store := &asyncMemoryStore{objects: map[string][]byte{}, mime: map[string]string{}}
	previousStore := GetAsyncObjectStore()
	SetAsyncObjectStore(store)
	asyncExecutorMu.RLock()
	previousExecutor := asyncRelayExecutor
	asyncExecutorMu.RUnlock()
	t.Cleanup(func() { SetAsyncObjectStore(previousStore); SetAsyncRelayExecutor(previousExecutor) })
	ref, err := store.Put(context.Background(), "async-worker/request", "application/json", strings.NewReader(`{"model":"image-model"}`), AsyncRelayMaxBytes)
	require.NoError(t, err)
	refBytes, err := common.Marshal(ref)
	require.NoError(t, err)
	now := time.Now().Unix()
	job := &model.AsyncJob{JobID: "async-worker", UserID: 9951, TokenID: 9952, IdempotencyScope: "worker-scope", IdempotencyKey: "worker-key", Fingerprint: "worker-request", Endpoint: "/v1/images/generations", Modality: "image", Model: "image-model", ContentType: "application/json", ClientIP: "203.0.113.8", RequestRef: string(refBytes), Status: model.AsyncJobQueued, NextRunAt: now, CreatedAt: now}
	_, _, err = model.CreateAsyncJob(context.Background(), job)
	require.NoError(t, err)
	claimed, err := model.ClaimAsyncJob(context.Background(), "worker-fixture", now)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	return claimed, store
}

func TestAsyncWorkerArchivesMultipleImagesWithoutRegeneration(t *testing.T) {
	job, store := asyncWorkerFixture(t)
	store.failArtifact = 1
	calls := 0
	SetAsyncRelayExecutor(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "Bearer async-worker-fixture", r.Header.Get("Authorization"))
		assert.Equal(t, "203.0.113.8:0", r.RemoteAddr)
		_, err := model.ApplyAsyncBilling(r.Context(), job.JobID, "settle", 10, 0, "wallet_only", job.Model)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		raw, err := common.Marshal(map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString([]byte("first-image"))}, map[string]any{"b64_json": base64.StdEncoding.EncodeToString([]byte("second-image"))}}})
		require.NoError(t, err)
		_, err = w.Write(raw)
		require.NoError(t, err)
	})
	runAsyncJob(context.Background(), job)
	got, err := model.GetAsyncJob(context.Background(), job.JobID)
	require.NoError(t, err)
	require.Equal(t, model.AsyncJobStoragePending, got.Status)
	require.NoError(t, model.DB.Model(got).Update("next_run_at", time.Now().Unix()).Error)
	retry, err := model.ClaimAsyncJob(context.Background(), "another-node", time.Now().Unix())
	require.NoError(t, err)
	require.NotNil(t, retry)
	runAsyncJob(context.Background(), retry)
	got, err = model.GetAsyncJob(context.Background(), job.JobID)
	require.NoError(t, err)
	assert.Equal(t, model.AsyncJobCompleted, got.Status)
	assert.Equal(t, 1, calls)
	var refs map[string]AsyncObjectRef
	require.NoError(t, common.UnmarshalJsonStr(got.ArtifactRefs, &refs))
	require.Len(t, refs, 2)
	assert.Equal(t, []byte("first-image"), store.objects[refs["image-1"].Key])
	assert.Equal(t, []byte("second-image"), store.objects[refs["image-2"].Key])
	var result AsyncObjectRef
	require.NoError(t, common.UnmarshalJsonStr(got.ResultRef, &result))
	assert.NotContains(t, string(store.objects[result.Key]), "b64_json")
	assert.Contains(t, string(store.objects[result.Key]), "/v1/async/tasks/async-worker/artifacts/image-2")
}

func TestAsyncWorkerBinaryArtifactSurvivesRawCleanup(t *testing.T) {
	job, store := asyncWorkerFixture(t)
	store.failArtifact = 1
	job.Modality = "audio"
	job.Endpoint = "/v1/audio/speech"
	require.NoError(t, model.DB.Model(job).Updates(map[string]any{"modality": job.Modality, "endpoint": job.Endpoint}).Error)
	calls := 0
	SetAsyncRelayExecutor(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, err := model.ApplyAsyncBilling(r.Context(), job.JobID, "settle", 0, 0, "wallet_only", job.Model)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, err = w.Write([]byte("audio-bytes"))
		require.NoError(t, err)
	})
	runAsyncJob(context.Background(), job)
	got, err := model.GetAsyncJob(context.Background(), job.JobID)
	require.NoError(t, err)
	require.Equal(t, model.AsyncJobStoragePending, got.Status)
	assert.Equal(t, []byte("audio-bytes"), store.objects["async-worker/raw-result"])
	require.NoError(t, model.DB.Model(got).Update("next_run_at", time.Now().Unix()).Error)
	retry, err := model.ClaimAsyncJob(context.Background(), "another-node", time.Now().Unix())
	require.NoError(t, err)
	require.NotNil(t, retry)
	runAsyncJob(context.Background(), retry)
	got, err = model.GetAsyncJob(context.Background(), job.JobID)
	require.NoError(t, err)
	require.Equal(t, model.AsyncJobCompleted, got.Status)
	var refs map[string]AsyncObjectRef
	require.NoError(t, common.UnmarshalJsonStr(got.ArtifactRefs, &refs))
	require.Contains(t, refs, "output")
	assert.Equal(t, []byte("audio-bytes"), store.objects[refs["output"].Key])
	var result AsyncObjectRef
	require.NoError(t, common.UnmarshalJsonStr(got.ResultRef, &result))
	assert.Equal(t, "application/json", result.ContentType)
	assert.NotEqual(t, refs["output"].Key, result.Key)
	var envelope struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Data   []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			MimeType string `json:"mime_type"`
			Size     int64  `json:"size"`
			URL      string `json:"url"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(store.objects[result.Key], &envelope))
	assert.Equal(t, job.JobID, envelope.ID)
	assert.Equal(t, "completed", envelope.Status)
	require.Len(t, envelope.Data, 1)
	assert.Equal(t, "audio", envelope.Data[0].Type)
	assert.Equal(t, "audio/mpeg", envelope.Data[0].MimeType)
	assert.EqualValues(t, len("audio-bytes"), envelope.Data[0].Size)
	assert.Equal(t, "/v1/async/tasks/async-worker/artifacts/output", envelope.Data[0].URL)
	assert.Equal(t, 1, calls)
	assert.NotContains(t, store.objects, "async-worker/raw-result")
}

func TestAsyncWorkerUnknownSubmissionCannotBeClaimedAgain(t *testing.T) {
	job, _ := asyncWorkerFixture(t)
	calls := 0
	SetAsyncRelayExecutor(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, err := model.ApplyAsyncBilling(r.Context(), job.JobID, model.AsyncReservePhase(100), 100, 0, "wallet_only", job.Model)
		require.NoError(t, err)
		w.WriteHeader(http.StatusBadGateway)
	})
	runAsyncJob(context.Background(), job)
	got, err := model.GetAsyncJob(context.Background(), job.JobID)
	require.NoError(t, err)
	assert.Equal(t, model.AsyncJobUnknown, got.Status)
	assert.Equal(t, "reserved", got.BillingState)
	claimed, err := model.ClaimAsyncJob(context.Background(), "another-node", time.Now().Unix()+100)
	require.NoError(t, err)
	assert.Nil(t, claimed)
	assert.Equal(t, 1, calls)
	assert.Positive(t, got.ResultExpiresAt)
}

func TestAsyncWorkerSingleDataObjectArchive(t *testing.T) {
	job, store := asyncWorkerFixture(t)
	SetAsyncRelayExecutor(func(w http.ResponseWriter, r *http.Request) {
		_, err := model.ApplyAsyncBilling(r.Context(), job.JobID, "settle", 0, 0, "wallet_only", job.Model)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		raw, err := common.Marshal(map[string]any{"data": map[string]any{"b64_json": base64.StdEncoding.EncodeToString([]byte("single-image"))}})
		require.NoError(t, err)
		_, err = w.Write(raw)
		require.NoError(t, err)
	})
	runAsyncJob(context.Background(), job)
	got, err := model.GetAsyncJob(context.Background(), job.JobID)
	require.NoError(t, err)
	assert.Equal(t, model.AsyncJobCompleted, got.Status)
	var refs map[string]AsyncObjectRef
	require.NoError(t, common.UnmarshalJsonStr(got.ArtifactRefs, &refs))
	require.Contains(t, refs, "image-1")
	assert.Equal(t, []byte("single-image"), store.objects[refs["image-1"].Key])
}

func TestAsyncWorkerRevokedKeyStopsSubmissionButNotInternalRead(t *testing.T) {
	job, _ := asyncWorkerFixture(t)
	require.NoError(t, model.DB.Delete(&model.Token{}, job.TokenID).Error)
	calls := 0
	SetAsyncRelayExecutor(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) })
	_, err := ExecuteAsyncRelay(context.Background(), job, "/v1/tasks/native/artifacts", http.MethodGet, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	runAsyncJob(context.Background(), job)
	got, err := model.GetAsyncJob(context.Background(), job.JobID)
	require.NoError(t, err)
	assert.Equal(t, model.AsyncJobFailed, got.Status)
	assert.Equal(t, "api_key_unavailable", got.ErrorCode)
	assert.Equal(t, 1, calls)
	assert.Zero(t, got.BillingReserved)
}

func TestAsyncWorkerReconcilesNativeTerminalCASAndArchivesAfterKeyDeletion(t *testing.T) {
	job, store := asyncWorkerFixture(t)
	ctx := context.Background()
	_, err := model.ApplyAsyncBilling(ctx, job.JobID, model.AsyncReservePhase(100), 100, 0, "wallet_only", job.Model)
	require.NoError(t, err)
	// Simulate a process dying after native terminal CAS but before its charge
	// adjustment. The persisted settlement intent allows another node to finish.
	require.NoError(t, model.SetAsyncSettlementIntent(ctx, job.JobID, 70))
	task := &model.Task{TaskID: "task_cas_crash", AsyncJobID: job.JobID, UserId: job.UserID, Platform: constant.TaskPlatform("test-native"), Status: model.TaskStatusSuccess, FinishTime: time.Now().Unix(), Quota: 100, Properties: model.Properties{OriginModelName: job.Model}, PrivateData: model.TaskPrivateData{TokenId: job.TokenID}}
	require.NoError(t, task.Insert())
	t.Cleanup(func() { model.DB.Delete(&model.Task{}, task.ID) })
	require.NoError(t, model.DB.Delete(&model.Token{}, job.TokenID).Error)
	job.Status = model.AsyncJobPolling
	job.NativeTaskID = task.TaskID
	require.NoError(t, model.UpdateAsyncJobLease(ctx, job, map[string]any{"status": job.Status, "native_task_id": task.TaskID}))
	posts := 0
	SetAsyncRelayExecutor(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			posts++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/artifacts") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"artifacts":[{"key":"video","type":"video","mime_type":"video/mp4"}]}`))
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("retained-video-bytes"))
	})
	runAsyncJob(ctx, job)
	got, err := model.GetAsyncJob(ctx, job.JobID)
	require.NoError(t, err)
	require.Equal(t, model.AsyncJobCompleted, got.Status)
	assert.Equal(t, "settled", got.BillingState)
	assert.Equal(t, 70, got.BillingQuota)
	assert.Zero(t, posts)
	var user model.User
	var token model.Token
	require.NoError(t, model.DB.First(&user, job.UserID).Error)
	require.NoError(t, model.DB.Unscoped().First(&token, job.TokenID).Error)
	assert.Equal(t, 930, user.Quota)
	assert.Equal(t, 70, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	assert.Equal(t, 930, token.RemainQuota)
	require.NoError(t, reconcileAsyncNativeBilling(ctx, task))
	var ledgerCount int64
	require.NoError(t, model.DB.Model(&model.AsyncJobLedger{}).Where("job_id = ? AND phase = ?", job.JobID, "settle").Count(&ledgerCount).Error)
	assert.EqualValues(t, 1, ledgerCount)
	var refs map[string]AsyncObjectRef
	require.NoError(t, common.UnmarshalJsonStr(got.ArtifactRefs, &refs))
	assert.Equal(t, []byte("retained-video-bytes"), store.objects[refs["video"].Key])
	// Content expiry cleans bytes but preserves reconciliable accounting.
	now := time.Now().Unix()
	require.NoError(t, model.DB.Model(got).Update("result_expires_at", now-1).Error)
	require.NoError(t, SweepAsyncJobs(ctx, now))
	got, err = model.GetAsyncJob(ctx, job.JobID)
	require.NoError(t, err)
	assert.Empty(t, got.RequestRef)
	assert.Empty(t, got.ResultRef)
	assert.Empty(t, got.ArtifactRefs)
	assert.Empty(t, store.objects)
	require.NoError(t, model.DB.Model(&model.AsyncJobLedger{}).Where("job_id = ? AND phase = ?", job.JobID, "settle").Count(&ledgerCount).Error)
	assert.EqualValues(t, 1, ledgerCount)
}

func TestAsyncWorkerAcceptedProviderIDRecoversFailedSQLInsertWithoutAnotherPOST(t *testing.T) {
	job, _ := asyncWorkerFixture(t)
	ctx := context.Background()
	posts := 0
	const callbackName = "async-test-native-insert-failure"
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Task" {
			tx.AddError(errors.New("simulated SQL outage after provider acceptance"))
		}
	}))
	t.Cleanup(func() { _ = model.DB.Callback().Create().Remove(callbackName) })
	SetAsyncRelayExecutor(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		posts++
		_, err := model.ApplyAsyncBilling(ctx, job.JobID, model.AsyncReservePhase(100), 100, 0, "wallet_only", job.Model)
		require.NoError(t, err)
		task := &model.Task{TaskID: "task_accepted_insert_failure", AsyncJobID: job.JobID, UserId: job.UserID, Platform: constant.TaskPlatform("test-native"), Status: model.TaskStatusSubmitted, Quota: 100, PrivateData: model.TaskPrivateData{UpstreamTaskID: "provider-already-started", TokenId: job.TokenID}}
		require.NoError(t, PersistAsyncNativeTaskSnapshot(ctx, task))
		require.ErrorContains(t, task.Insert(), "simulated SQL outage")
		w.WriteHeader(http.StatusInternalServerError)
	})
	runAsyncJob(ctx, job)
	got, err := model.GetAsyncJob(ctx, job.JobID)
	require.NoError(t, err)
	require.Equal(t, model.AsyncJobUnknown, got.Status)
	assert.Equal(t, "reserved", got.BillingState)
	now := time.Now().Unix()
	claimed, err := model.ClaimAsyncJob(ctx, "another-node", now)
	require.NoError(t, err)
	assert.Nil(t, claimed)
	require.NoError(t, model.DB.Callback().Create().Remove(callbackName))
	require.NoError(t, SweepAsyncJobs(ctx, now))
	got, err = model.GetAsyncJob(ctx, job.JobID)
	require.NoError(t, err)
	require.Equal(t, model.AsyncJobPolling, got.Status)
	assert.Equal(t, "task_accepted_insert_failure", got.NativeTaskID)
	var task model.Task
	require.NoError(t, model.DB.Where("async_job_id = ?", job.JobID).First(&task).Error)
	t.Cleanup(func() { model.DB.Delete(&model.Task{}, task.ID) })
	assert.Equal(t, "provider-already-started", task.GetUpstreamTaskID())
	claimed, err = model.ClaimAsyncJob(ctx, "another-node", now)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	runAsyncJob(ctx, claimed)
	assert.Equal(t, 1, posts)
	got, err = model.GetAsyncJob(ctx, job.JobID)
	require.NoError(t, err)
	assert.Equal(t, model.AsyncJobPolling, got.Status)
	assert.Equal(t, "reserved", got.BillingState)
}

func TestAsyncWorkerUnknownReceiptRecoveryRotatesPastMissingFirstBatch(t *testing.T) {
	job, _ := asyncWorkerFixture(t)
	ctx := context.Background()
	now := time.Now().Unix()
	require.NoError(t, model.DB.Model(job).Updates(map[string]any{"status": model.AsyncJobUnknown, "error_code": "submission_outcome_unknown", "lease_until": 0, "lease_owner": "", "next_run_at": now}).Error)
	for i := range 24 {
		missing := &model.AsyncJob{JobID: fmt.Sprintf("async-missing-receipt-%d", i), UserID: job.UserID, TokenID: job.TokenID, IdempotencyScope: "missing-receipt-scope", IdempotencyKey: fmt.Sprintf("missing-%d", i), Fingerprint: "test", Status: model.AsyncJobUnknown, ErrorCode: "submission_outcome_unknown", NextRunAt: now, CreatedAt: now}
		require.NoError(t, model.DB.Create(missing).Error)
	}
	recoverable := &model.AsyncJob{JobID: "async-later-native-receipt", UserID: job.UserID, TokenID: job.TokenID, IdempotencyScope: "late-receipt-scope", IdempotencyKey: "late", Fingerprint: "test", Status: model.AsyncJobUnknown, ErrorCode: "submission_outcome_unknown", NextRunAt: now, CreatedAt: now}
	require.NoError(t, model.DB.Create(recoverable).Error)
	task := &model.Task{TaskID: "task_late_receipt", AsyncJobID: recoverable.JobID, UserId: job.UserID, Platform: constant.TaskPlatform("test-native"), Status: model.TaskStatusSubmitted, PrivateData: model.TaskPrivateData{UpstreamTaskID: "late-provider-id"}}
	require.NoError(t, PersistAsyncNativeTaskReceipt(ctx, task))
	require.NoError(t, SweepAsyncJobs(ctx, now))
	first, err := model.GetAsyncJob(ctx, job.JobID)
	require.NoError(t, err)
	assert.Equal(t, now+60, first.NextRunAt)
	require.NoError(t, SweepAsyncJobs(ctx, now))
	got, err := model.GetAsyncJob(ctx, recoverable.JobID)
	require.NoError(t, err)
	assert.Equal(t, model.AsyncJobPolling, got.Status)
	assert.Equal(t, task.TaskID, got.NativeTaskID)
	t.Cleanup(func() { model.DB.Where("async_job_id = ?", recoverable.JobID).Delete(&model.Task{}) })
}

func TestAsyncWorkerUnsubmittedFailureFinalizesZeroJournalAndExpiresSummary(t *testing.T) {
	for _, reason := range []string{"queued_expired", "invalid_request_ref"} {
		t.Run(reason, func(t *testing.T) {
			job, store := asyncWorkerFixture(t)
			ctx := context.Background()
			if reason == "queued_expired" {
				job.CreatedAt = time.Now().Unix() - 25*3600
				require.NoError(t, model.DB.Model(job).Update("created_at", job.CreatedAt).Error)
			} else {
				job.RequestRef = "invalid-json"
				require.NoError(t, model.DB.Model(job).Update("request_ref", job.RequestRef).Error)
			}
			calls := 0
			SetAsyncRelayExecutor(func(w http.ResponseWriter, r *http.Request) { calls++ })
			runAsyncJob(ctx, job)
			got, err := model.GetAsyncJob(ctx, job.JobID)
			require.NoError(t, err)
			assert.Equal(t, model.AsyncJobFailed, got.Status)
			assert.Equal(t, "refunded", got.BillingState)
			assert.Zero(t, got.BillingQuota)
			assert.Zero(t, calls)
			// The tracked store handles invalid/orphan object refs too; this
			// admission fixture is untracked, so only assert the valid-ref case.
			if reason == "queued_expired" {
				require.NoError(t, SweepAsyncJobs(ctx, got.ResultExpiresAt+1))
				assert.Empty(t, store.objects)
			}
			require.NoError(t, SweepAsyncJobs(ctx, got.SummaryExpiresAt+1))
			_, err = model.GetAsyncJob(ctx, job.JobID)
			require.ErrorIs(t, err, gorm.ErrRecordNotFound)
			var ledger int64
			require.NoError(t, model.DB.Model(&model.AsyncJobLedger{}).Where("job_id = ? AND phase = ?", job.JobID, "refund").Count(&ledger).Error)
			assert.EqualValues(t, 1, ledger)
		})
	}
}
