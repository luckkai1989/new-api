package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type asyncCleanupFailStore struct {
	AsyncObjectStore
	deletes int
}

func (s *asyncCleanupFailStore) Delete(ctx context.Context, ref AsyncObjectRef) error {
	if s.deletes > 0 {
		s.deletes--
		return errors.New("test delete outage")
	}
	return s.AsyncObjectStore.Delete(ctx, ref)
}

func TestAsyncTrackedObjectCleanupLiveOrphanExpiryAndRetry(t *testing.T) {
	job, memory := asyncWorkerFixture(t)
	ctx := context.Background()
	now := time.Now().Unix()
	flaky := &asyncCleanupFailStore{AsyncObjectStore: memory}
	store := &trackedAsyncObjectStore{AsyncObjectStore: flaky}
	SetAsyncObjectStore(store)
	for _, key := range []string{job.JobID + "/live", "orphan-job/input"} {
		_, err := store.Put(ctx, key, "application/octet-stream", strings.NewReader("test"), 16)
		require.NoError(t, err)
	}
	require.NoError(t, model.DB.Model(&model.AsyncObjectCleanup{}).Where("next_check_at > ?", now).Update("next_check_at", now).Error)
	flaky.deletes = 1
	require.NoError(t, SweepAsyncTrackedObjects(ctx, now))
	assert.Contains(t, memory.objects, "orphan-job/input")
	require.NoError(t, SweepAsyncTrackedObjects(ctx, now+61))
	assert.NotContains(t, memory.objects, "orphan-job/input")
	assert.Contains(t, memory.objects, job.JobID+"/live")
	require.NoError(t, model.DB.Model(&model.AsyncJob{}).Where("id = ?", job.ID).Updates(map[string]any{"status": model.AsyncJobCompleted, "result_expires_at": now - 1}).Error)
	require.NoError(t, model.DB.Model(&model.AsyncObjectCleanup{}).Where("job_id = ?", job.JobID).Update("next_check_at", now).Error)
	require.NoError(t, SweepAsyncTrackedObjects(ctx, now+62))
	assert.NotContains(t, memory.objects, job.JobID+"/live")
	var count int64
	require.NoError(t, model.DB.Model(&model.AsyncObjectCleanup{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestAsyncTrackedObjectCleanupRetainsLiveJobsPastProvisionalDeadline(t *testing.T) {
	job, memory := asyncWorkerFixture(t)
	ctx := context.Background()
	now := time.Now().Unix()
	store := &trackedAsyncObjectStore{AsyncObjectStore: memory}
	SetAsyncObjectStore(store)
	keys := []string{job.JobID + "/request", job.JobID + "/native-snapshots/test", job.JobID + "/native-receipt"}
	for _, key := range keys {
		_, err := store.Put(ctx, key, "application/octet-stream", strings.NewReader("recovery-bytes"), 64)
		require.NoError(t, err)
	}
	for _, status := range []string{model.AsyncJobQueued, model.AsyncJobSubmitting, model.AsyncJobPolling} {
		require.NoError(t, model.DB.Model(job).Updates(map[string]any{"status": status, "result_expires_at": now - 1}).Error)
		require.NoError(t, model.DB.Model(&model.AsyncObjectCleanup{}).Where("job_id = ?", job.JobID).Update("next_check_at", now).Error)
		require.NoError(t, SweepAsyncTrackedObjects(ctx, now))
		for _, key := range keys {
			assert.Contains(t, memory.objects, key, status+" must keep recovery objects even with an obsolete deadline")
		}
	}
	require.NoError(t, model.DB.Model(job).Update("status", model.AsyncJobCompleted).Error)
	require.NoError(t, model.DB.Model(&model.AsyncObjectCleanup{}).Where("job_id = ?", job.JobID).Update("next_check_at", now).Error)
	require.NoError(t, SweepAsyncTrackedObjects(ctx, now))
	assert.Empty(t, memory.objects)
}
