package service

import (
	"context"
	"errors"
	"io"

	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

type trackedAsyncObjectStore struct{ AsyncObjectStore }

func (s *trackedAsyncObjectStore) Lookup(ctx context.Context, key string, maxBytes int64) (AsyncObjectRef, error) {
	lookup, ok := s.AsyncObjectStore.(asyncRecoveryObjectLookup)
	if !ok {
		return AsyncObjectRef{}, errors.New("async storage cannot look up a recovery object")
	}
	return lookup.Lookup(ctx, key, maxBytes)
}

func (s *trackedAsyncObjectStore) Put(ctx context.Context, key, contentType string, reader io.Reader, maxBytes int64) (AsyncObjectRef, error) {
	if !validAsyncObjectKey(key) {
		return AsyncObjectRef{}, errors.New("invalid async object key")
	}
	if err := model.TrackAsyncObject(ctx, key); err != nil {
		return AsyncObjectRef{}, errors.New("could not register durable async object cleanup")
	}
	return s.AsyncObjectStore.Put(ctx, key, contentType, reader, maxBytes)
}

func (s *trackedAsyncObjectStore) Delete(ctx context.Context, ref AsyncObjectRef) error {
	if err := s.AsyncObjectStore.Delete(ctx, ref); err != nil {
		return err
	}
	return model.ForgetAsyncObject(ctx, ref.Key)
}

// SweepAsyncTrackedObjects cleans uploads absent from job refs as well as
// partially archived results. Live jobs retain their inputs until completion.
func SweepAsyncTrackedObjects(ctx context.Context, now int64) error {
	store := GetAsyncObjectStore()
	if !store.Enabled() {
		return nil
	}
	var entries []model.AsyncObjectCleanup
	if err := model.DB.WithContext(ctx).Where("next_check_at <= ?", now).Order("id").Limit(40).Find(&entries).Error; err != nil {
		return err
	}
	for _, entry := range entries {
		job, err := model.GetAsyncJob(ctx, entry.JobID)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		live := err == nil && (job.Status == model.AsyncJobQueued || job.Status == model.AsyncJobSubmitting || job.Status == model.AsyncJobPolling)
		if err == nil && (live || job.ResultExpiresAt == 0 || job.ResultExpiresAt > now) {
			next := now + 3600
			if !live && job.ResultExpiresAt > now {
				next = job.ResultExpiresAt
			}
			if err := model.DB.WithContext(ctx).Model(&entry).Update("next_check_at", next).Error; err != nil {
				return err
			}
			continue
		}
		if err := store.Delete(ctx, AsyncObjectRef{Backend: "r2", Key: entry.ObjectKey}); err != nil {
			if updateErr := model.DB.WithContext(ctx).Model(&entry).Update("next_check_at", now+60).Error; updateErr != nil {
				return updateErr
			}
			continue
		}
		if err := model.ForgetAsyncObject(ctx, entry.ObjectKey); err != nil {
			return err
		}
	}
	return nil
}
