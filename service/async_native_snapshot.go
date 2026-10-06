package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type asyncNativeSnapshot struct {
	Version     int             `json:"version"`
	JobID       string          `json:"job_id"`
	TaskID      string          `json:"task_id"`
	Data        json.RawMessage `json:"data,omitempty"`
	PluginState json.RawMessage `json:"plugin_state,omitempty"`
	Input       string          `json:"input,omitempty"`
	ResultURL   string          `json:"result_url,omitempty"`
	FailReason  string          `json:"fail_reason,omitempty"`
}

// PersistAsyncNativeTaskSnapshot runs before SQL INSERT/CAS, never inside a
// database transaction. Unique immutable keys prevent a losing CAS writer from
// overwriting the winning snapshot; tracked R2 cleanup also handles orphans.
// Payloads remain in this request's task for presentation and billing only.
func PersistAsyncNativeTaskSnapshot(ctx context.Context, task *model.Task) error {
	if task == nil || task.AsyncJobID == "" {
		return nil
	}
	store := GetAsyncObjectStore()
	if !store.Enabled() {
		return ErrAsyncObjectStoreDisabled
	}
	// Preserve the accepted upstream ID independently of the large snapshot
	// so even a blob upload or SQL INSERT failure cannot force resubmission.
	if err := PersistAsyncNativeTaskReceipt(ctx, task); err != nil {
		return err
	}
	snapshot := asyncNativeSnapshot{Version: 1, JobID: task.AsyncJobID, TaskID: task.TaskID,
		Data: task.Data, PluginState: task.PrivateData.PluginState, Input: task.Properties.Input,
		ResultURL: task.PrivateData.ResultURL, FailReason: task.FailReason}
	raw, err := common.Marshal(snapshot)
	if err != nil {
		return errors.New("invalid async native snapshot")
	}
	if int64(len(raw)) > AsyncRelayMaxBytes {
		return errors.New("async native snapshot exceeds the media limit")
	}
	key := task.AsyncJobID + "/native-snapshots/" + common.GetRandomString(32)
	ref, err := store.Put(ctx, key, "application/json", bytes.NewReader(raw), AsyncRelayMaxBytes)
	if err != nil {
		return errors.New("could not persist async native snapshot")
	}
	task.PrivateData.AsyncSnapshotRef = &ref
	task.AsyncSnapshotHydrated = true
	return PersistAsyncNativeTaskReceipt(ctx, task)
}

// HydrateAsyncNativeTask is an explicit read at an already-authorized execution,
// protocol, billing or artifact boundary. It does not write SQL, use AfterFind,
// or change historical non-job tasks. A task object is hydrated only once so a
// later presenter cannot overwrite an in-memory poller's newly parsed state.
func HydrateAsyncNativeTask(ctx context.Context, task *model.Task) error {
	if task == nil || task.AsyncJobID == "" || task.PrivateData.AsyncSnapshotRef == nil || task.AsyncSnapshotHydrated {
		return nil
	}
	ref := *task.PrivateData.AsyncSnapshotRef
	if ref.Backend != "r2" || !strings.HasPrefix(ref.Key, task.AsyncJobID+"/native-snapshots/") || !validAsyncObjectKey(ref.Key) || ref.Size < 0 || ref.Size > AsyncRelayMaxBytes {
		return errors.New("invalid async native snapshot reference")
	}
	readContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, _, _, err := GetAsyncObjectStore().Open(readContext, ref)
	if err != nil {
		return errors.New("async native snapshot is unavailable")
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, AsyncRelayMaxBytes+1))
	if err != nil || int64(len(raw)) > AsyncRelayMaxBytes {
		return errors.New("async native snapshot could not be read")
	}
	var snapshot asyncNativeSnapshot
	if common.Unmarshal(raw, &snapshot) != nil || snapshot.Version != 1 || snapshot.JobID != task.AsyncJobID || snapshot.TaskID != task.TaskID {
		return errors.New("invalid async native snapshot identity")
	}
	task.Data = snapshot.Data
	task.PrivateData.PluginState = snapshot.PluginState
	task.Properties.Input = snapshot.Input
	task.PrivateData.ResultURL = snapshot.ResultURL
	// SQL owns current failure/status transitions; a poll timeout may have
	// changed its reason without writing a new generated payload snapshot.
	if task.FailReason == "" {
		task.FailReason = snapshot.FailReason
	}
	task.AsyncSnapshotHydrated = true
	return nil
}
