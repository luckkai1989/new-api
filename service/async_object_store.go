package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/model"
)

// AsyncObjectRef never contains credentials or a public object URL.
type AsyncObjectRef = model.AsyncObjectRef

type AsyncObjectStore interface {
	Enabled() bool
	Put(context.Context, string, string, io.Reader, int64) (AsyncObjectRef, error)
	Open(context.Context, AsyncObjectRef) (io.ReadCloser, string, int64, error)
	Delete(context.Context, AsyncObjectRef) error
}

type asyncRecoveryObjectLookup interface {
	Lookup(context.Context, string, int64) (AsyncObjectRef, error)
}

// Recovery receipts use a server-derived key whose size may not have reached
// SQL before a crash. Discover the size with authenticated HEAD, then keep the
// normal GET/reference size check. This is never an arbitrary caller lookup.
func OpenAsyncRecoveryObject(ctx context.Context, key string, maxBytes int64) (io.ReadCloser, string, int64, error) {
	if !validAsyncObjectKey(key) || !strings.HasSuffix(key, "/native-receipt") || maxBytes <= 0 || maxBytes > AsyncObjectMaxBytes {
		return nil, "", 0, errors.New("invalid async recovery object")
	}
	store := GetAsyncObjectStore()
	lookup, ok := store.(asyncRecoveryObjectLookup)
	if !ok {
		return nil, "", 0, errors.New("async storage cannot look up a recovery object")
	}
	ref, err := lookup.Lookup(ctx, key, maxBytes)
	if err != nil {
		return nil, "", 0, err
	}
	return store.Open(ctx, ref)
}

var ErrAsyncObjectStoreDisabled = errors.New("async tasks require a configured private R2 bucket")

type disabledAsyncObjectStore struct{}

func (disabledAsyncObjectStore) Enabled() bool { return false }
func (disabledAsyncObjectStore) Put(context.Context, string, string, io.Reader, int64) (AsyncObjectRef, error) {
	return AsyncObjectRef{}, ErrAsyncObjectStoreDisabled
}
func (disabledAsyncObjectStore) Open(context.Context, AsyncObjectRef) (io.ReadCloser, string, int64, error) {
	return nil, "", 0, ErrAsyncObjectStoreDisabled
}
func (disabledAsyncObjectStore) Delete(context.Context, AsyncObjectRef) error {
	return ErrAsyncObjectStoreDisabled
}

var asyncObjectStoreMu sync.RWMutex
var asyncObjectStore AsyncObjectStore = disabledAsyncObjectStore{}

func SetAsyncObjectStore(store AsyncObjectStore) {
	asyncObjectStoreMu.Lock()
	defer asyncObjectStoreMu.Unlock()
	if store == nil {
		store = disabledAsyncObjectStore{}
	}
	asyncObjectStore = store
}

func GetAsyncObjectStore() AsyncObjectStore {
	asyncObjectStoreMu.RLock()
	defer asyncObjectStoreMu.RUnlock()
	return asyncObjectStore
}
