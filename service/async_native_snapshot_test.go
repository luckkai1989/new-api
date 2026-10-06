package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsyncNativeSnapshotKeepsSQLMetadataAndCASWinner(t *testing.T) {
	job, store := asyncWorkerFixture(t)
	ctx := context.Background()
	task := &model.Task{TaskID: "native-snapshot-fixture", UserId: job.UserID, AsyncJobID: job.JobID,
		Status: model.TaskStatusSubmitted, Data: json.RawMessage(`{"b64_json":"private-generated-canary"}`),
		Properties:  model.Properties{Input: "private-input-canary"},
		PrivateData: model.TaskPrivateData{PluginState: json.RawMessage(`{"cursor":"private-state-canary"}`), ResultURL: "https://upstream.invalid/private-generated-canary"}}
	require.Error(t, task.Insert(), "raw job-backed snapshots must never be written without durable R2")
	require.NoError(t, PersistAsyncNativeTaskSnapshot(ctx, task))
	require.NotNil(t, task.PrivateData.AsyncSnapshotRef)
	firstKey := task.PrivateData.AsyncSnapshotRef.Key
	require.NoError(t, task.Insert())
	t.Cleanup(func() { model.DB.Delete(&model.Task{}, task.ID) })
	assert.Contains(t, string(store.objects[firstKey]), "private-generated-canary")
	assert.Contains(t, string(task.Data), "private-generated-canary", "the submit presenter retains its in-memory result")
	load := func() *model.Task {
		var loaded model.Task
		require.NoError(t, model.DB.First(&loaded, task.ID).Error)
		assert.Empty(t, loaded.Data)
		assert.Empty(t, loaded.PrivateData.PluginState)
		assert.Empty(t, loaded.PrivateData.ResultURL)
		assert.Empty(t, loaded.Properties.Input)
		private, err := common.Marshal(loaded.PrivateData)
		require.NoError(t, err)
		assert.NotContains(t, string(private), "private-generated-canary")
		assert.NotContains(t, string(private), "private-state-canary")
		require.NoError(t, HydrateAsyncNativeTask(ctx, &loaded))
		return &loaded
	}
	// Protocol background flags use a direct private_data update; its Valuer
	// must strip hydrated payloads just like the complete Task persistence path.
	task.PrivateData.ResponsesBackground = true
	require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
	background := load()
	assert.True(t, background.PrivateData.ResponsesBackground)
	winner, loser := load(), load()
	assert.Equal(t, task.Data, winner.Data)
	assert.Equal(t, task.Properties.Input, winner.Properties.Input)
	assert.Equal(t, task.PrivateData.PluginState, winner.PrivateData.PluginState)
	winner.Status = model.TaskStatusSuccess
	winner.Data = json.RawMessage(`{"b64_json":"winning-generation"}`)
	winner.FinishTime = time.Now().Unix()
	require.NoError(t, PersistAsyncNativeTaskSnapshot(ctx, winner))
	won, err := winner.UpdateWithStatus(model.TaskStatusSubmitted)
	require.NoError(t, err)
	require.True(t, won)
	loser.Status = model.TaskStatusSuccess
	loser.Data = json.RawMessage(`{"b64_json":"losing-stale-result"}`)
	require.NoError(t, PersistAsyncNativeTaskSnapshot(ctx, loser))
	assert.NotEqual(t, winner.PrivateData.AsyncSnapshotRef.Key, loser.PrivateData.AsyncSnapshotRef.Key)
	won, err = loser.UpdateWithStatus(model.TaskStatusSubmitted)
	require.NoError(t, err)
	assert.False(t, won)
	current := load()
	assert.JSONEq(t, `{"b64_json":"winning-generation"}`, string(current.Data))
	current.Data = json.RawMessage(`{"b64_json":"new-in-memory-state"}`)
	require.NoError(t, HydrateAsyncNativeTask(ctx, current))
	assert.JSONEq(t, `{"b64_json":"new-in-memory-state"}`, string(current.Data), "repeat hydration must not overwrite a current parser's changes")
	foreign := *current
	foreign.AsyncJobID = "other-job"
	foreign.AsyncSnapshotHydrated = false
	require.Error(t, HydrateAsyncNativeTask(ctx, &foreign))
}

func TestAsyncNativeSnapshotLeavesLegacyPersistenceUnchanged(t *testing.T) {
	previous := GetAsyncObjectStore()
	SetAsyncObjectStore(nil)
	t.Cleanup(func() { SetAsyncObjectStore(previous) })
	task := &model.Task{TaskID: "legacy-snapshot-fixture", Status: model.TaskStatusSubmitted,
		Data:        json.RawMessage(`{"url":"https://legacy.invalid/image"}`),
		PrivateData: model.TaskPrivateData{PluginState: json.RawMessage(`{"cursor":"legacy"}`)}}
	require.NoError(t, PersistAsyncNativeTaskSnapshot(context.Background(), task))
	require.NoError(t, task.Insert())
	t.Cleanup(func() { model.DB.Delete(&model.Task{}, task.ID) })
	var stored model.Task
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	require.NoError(t, HydrateAsyncNativeTask(context.Background(), &stored))
	assert.Equal(t, task.Data, stored.Data)
	assert.Equal(t, task.PrivateData.PluginState, stored.PrivateData.PluginState)
	assert.Nil(t, stored.PrivateData.AsyncSnapshotRef)
}

func TestAsyncNativeSnapshotFailureRetainsProviderRecoveryIdentity(t *testing.T) {
	job, _ := asyncWorkerFixture(t)
	SetAsyncObjectStore(nil)
	task := &model.Task{TaskID: "native-snapshot-recovery", UserId: job.UserID, AsyncJobID: job.JobID,
		Status: model.TaskStatusSubmitted, Data: json.RawMessage(`{"b64_json":"must-not-enter-sql"}`),
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "provider-accepted-id", PluginState: json.RawMessage(`{"payload":"must-not-enter-sql"}`)}}
	require.Error(t, PersistAsyncNativeTaskSnapshot(context.Background(), task))
	summary := task.AsyncNativeSummary()
	require.NoError(t, summary.Insert())
	t.Cleanup(func() { model.DB.Delete(&model.Task{}, summary.ID) })
	var stored model.Task
	require.NoError(t, model.DB.Where("async_job_id = ?", job.JobID).First(&stored).Error)
	assert.Equal(t, "provider-accepted-id", stored.PrivateData.UpstreamTaskID)
	assert.EqualValues(t, model.TaskStatusSubmitted, stored.Status)
	assert.Empty(t, stored.Data)
	assert.Empty(t, stored.PrivateData.PluginState)
	assert.Nil(t, stored.PrivateData.AsyncSnapshotRef)
	assert.NotEmpty(t, task.Data, "metadata recovery never loses the request-local result")
}
