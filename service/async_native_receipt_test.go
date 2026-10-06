package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsyncNativeReceiptRecoveryUsesRealR2HEADAndGET(t *testing.T) {
	job, _ := asyncWorkerFixture(t)
	var mu sync.Mutex
	objects := map[string][]byte{}
	heads, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		assert.Contains(t, r.Header.Get("Authorization"), "AWS4-HMAC-SHA256")
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			objects[r.URL.Path] = body
		case http.MethodHead, http.MethodGet:
			body, exists := objects[r.URL.Path]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method == http.MethodHead {
				heads++
			} else {
				gets++
				_, _ = w.Write(body)
			}
		}
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	store := &asyncR2Store{endpoint: endpoint, bucket: "private-jobs", prefix: "new-api-async", credentials: aws.Credentials{AccessKeyID: "test-key", SecretAccessKey: "test-secret"}, client: server.Client(), signer: v4.NewSigner()}
	SetAsyncObjectStore(store)
	task := &model.Task{TaskID: "task_real_r2_receipt", AsyncJobID: job.JobID, UserId: job.UserID, ChannelId: job.ChannelID, Platform: constant.TaskPlatform("test-native"), Status: model.TaskStatusSubmitted, PrivateData: model.TaskPrivateData{UpstreamTaskID: "provider-real-r2"}}
	require.NoError(t, PersistAsyncNativeTaskReceipt(t.Context(), task))
	result, err := RecoverAsyncNativeTaskReceipt(t.Context(), job)
	require.NoError(t, err)
	t.Cleanup(func() { model.DB.Delete(&model.Task{}, result.ID) })
	assert.Equal(t, "provider-real-r2", result.GetUpstreamTaskID())
	assert.Empty(t, result.Data)
	assert.Equal(t, 1, heads)
	assert.Equal(t, 1, gets)
	// Existing SQL is authoritative: recovery must not fetch a stale receipt.
	second, err := RecoverAsyncNativeTaskReceipt(t.Context(), job)
	require.NoError(t, err)
	assert.Equal(t, result.ID, second.ID)
	assert.Equal(t, 1, gets)
}

func TestAsyncNativeReceiptRecoversInsertFailureWithoutCredentialsOrSubmission(t *testing.T) {
	job, store := asyncWorkerFixture(t)
	ctx := context.Background()
	task := &model.Task{TaskID: "task_receipt_recovered", AsyncJobID: job.JobID, UserId: job.UserID, ChannelId: job.ChannelID, Platform: constant.TaskPlatform("test-native"), Status: model.TaskStatusSubmitted, Data: json.RawMessage(`{"result":"large-media-canary"}`), Properties: model.Properties{Input: "input-media-canary"}, PrivateData: model.TaskPrivateData{Key: "provider-secret-canary", UpstreamTaskID: "provider-task-123", TokenId: job.TokenID}}
	require.NoError(t, PersistAsyncNativeTaskReceipt(ctx, task))
	raw := store.objects[job.JobID+"/native-receipt"]
	assert.NotContains(t, string(raw), "provider-secret-canary")
	assert.NotContains(t, string(raw), "large-media-canary")
	assert.NotContains(t, string(raw), "input-media-canary")
	assert.Contains(t, string(raw), "provider-task-123")
	require.NoError(t, PersistAsyncNativeTaskSnapshot(ctx, task))
	require.NoError(t, PersistAsyncNativeTaskReceipt(ctx, task))
	// No Task INSERT occurred; the SQL row is rebuilt solely from an accepted
	// provider receipt. Neither this recovery nor its replay dispatches a POST.
	calls := 0
	SetAsyncRelayExecutor(func(_ http.ResponseWriter, _ *http.Request) { calls++ })
	recovered, err := RecoverAsyncNativeTaskReceipt(ctx, job)
	require.NoError(t, err)
	t.Cleanup(func() { model.DB.Delete(&model.Task{}, recovered.ID) })
	assert.Equal(t, task.TaskID, recovered.TaskID)
	assert.Equal(t, job.JobID, recovered.AsyncJobID)
	assert.Equal(t, "provider-task-123", recovered.GetUpstreamTaskID())
	assert.Empty(t, recovered.PrivateData.Key)
	require.NoError(t, HydrateAsyncNativeTask(ctx, recovered))
	assert.JSONEq(t, string(task.Data), string(recovered.Data))
	copy, err := RecoverAsyncNativeTaskReceipt(ctx, job)
	require.NoError(t, err)
	assert.Equal(t, recovered.ID, copy.ID)
	assert.Zero(t, calls)
}

func TestAsyncNativeReceiptRejectsCollisionAndForeignScope(t *testing.T) {
	job, store := asyncWorkerFixture(t)
	ctx := context.Background()
	task := &model.Task{TaskID: "task_receipt_collision", AsyncJobID: job.JobID, UserId: job.UserID, ChannelId: job.ChannelID, Platform: constant.TaskPlatform("test-native"), Status: model.TaskStatusSubmitted, PrivateData: model.TaskPrivateData{UpstreamTaskID: "provider-task-123"}}
	require.NoError(t, PersistAsyncNativeTaskReceipt(ctx, task))
	existing := &model.Task{TaskID: task.TaskID, UserId: 123, Platform: task.Platform, Status: model.TaskStatusSubmitted}
	require.NoError(t, existing.Insert())
	t.Cleanup(func() { model.DB.Delete(&model.Task{}, existing.ID) })
	_, err := RecoverAsyncNativeTaskReceipt(ctx, job)
	require.ErrorContains(t, err, "collision")
	var receipt asyncNativeReceipt
	require.NoError(t, common.Unmarshal(store.objects[job.JobID+"/native-receipt"], &receipt))
	receipt.Task.UserId = job.UserID + 1
	encoded, err := common.Marshal(receipt)
	require.NoError(t, err)
	store.objects[job.JobID+"/native-receipt"] = encoded
	_, err = RecoverAsyncNativeTaskReceipt(ctx, job)
	require.ErrorContains(t, err, "identity mismatch")
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, existing.ID).Error)
	assert.Equal(t, 123, persisted.UserId)
	assert.Empty(t, persisted.AsyncJobID)
}
