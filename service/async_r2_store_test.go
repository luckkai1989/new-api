package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsyncR2ConfigurationRejectsNonR2AndPartialCredentials(t *testing.T) {
	valid := AsyncR2Config{Endpoint: "https://test-account.r2.cloudflarestorage.com", Bucket: "private-jobs", AccessKey: "test-key", SecretKey: "test-secret"}
	store, err := NewAsyncR2Store(valid)
	require.NoError(t, err)
	assert.True(t, store.Enabled())
	for _, endpoint := range []string{"http://test-account.r2.cloudflarestorage.com", "https://127.0.0.1", "https://test-account.r2.cloudflarestorage.com.evil.test", "https://user:password@test-account.r2.cloudflarestorage.com", valid.Endpoint + "?secret=value", valid.Endpoint + "/prefix"} {
		config := valid
		config.Endpoint = endpoint
		_, err := NewAsyncR2Store(config)
		assert.Error(t, err)
	}
	valid.SecretKey = ""
	_, err = NewAsyncR2Store(valid)
	assert.Error(t, err)
	SetAsyncObjectStore(nil)
	t.Cleanup(func() { SetAsyncObjectStore(nil) })
	assert.False(t, GetAsyncObjectStore().Enabled())
	_, err = GetAsyncObjectStore().Put(t.Context(), "job/input", "", strings.NewReader("input"), 64)
	assert.ErrorIs(t, err, ErrAsyncObjectStoreDisabled)
}

func TestAsyncR2PrivateObjectRoundTripIsSignedBoundedAndIdempotent(t *testing.T) {
	objects := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 "))
		assert.NotEmpty(t, r.Header.Get("X-Amz-Date"))
		assert.NotEmpty(t, r.Header.Get("X-Amz-Content-Sha256"))
		assert.Equal(t, "/private-jobs/new-api-async/jobs/example/result.json", r.URL.Path)
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			objects[r.URL.Path] = string(body)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			body, exists := objects[r.URL.Path]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, body)
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	store := &asyncR2Store{endpoint: endpoint, bucket: "private-jobs", prefix: "new-api-async", credentials: aws.Credentials{AccessKeyID: "test-key", SecretAccessKey: "test-secret"}, client: server.Client(), signer: v4.NewSigner()}
	for range 2 {
		ref, err := store.Put(t.Context(), "jobs/example/result.json", "application/json", strings.NewReader(`{"ok":true}`), 64)
		require.NoError(t, err)
		assert.Equal(t, "r2", ref.Backend)
		assert.Equal(t, int64(11), ref.Size)
		body, contentType, size, err := store.Open(t.Context(), ref)
		require.NoError(t, err)
		data, err := io.ReadAll(body)
		require.NoError(t, err)
		require.NoError(t, body.Close())
		assert.Equal(t, `{"ok":true}`, string(data))
		assert.Equal(t, "application/json", contentType)
		assert.Equal(t, int64(11), size)
	}
	assert.Len(t, objects, 1)
	ref := AsyncObjectRef{Backend: "r2", Key: "jobs/example/result.json", Size: 11}
	badRef := ref
	badRef.Size = 10
	_, _, _, err = store.Open(t.Context(), badRef)
	assert.Error(t, err)
	for _, key := range []string{"../result", "jobs/../result", "/result", "jobs//result", "jobs\\result", "jobs/result?public=yes"} {
		_, err = store.Put(t.Context(), key, "", strings.NewReader("input"), 64)
		assert.Error(t, err)
	}
	_, err = store.Put(t.Context(), ref.Key, "", strings.NewReader("too large"), 3)
	assert.Error(t, err)
	assert.Equal(t, `{"ok":true}`, objects["/private-jobs/new-api-async/jobs/example/result.json"])
	require.NoError(t, store.Delete(t.Context(), ref))
	require.NoError(t, store.Delete(t.Context(), ref))
	assert.Empty(t, objects)
}

func TestAsyncR2TransportDoesNotFollowRedirectsOrExposeProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1/private")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, "provider response contains secret-test")
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	store := &asyncR2Store{endpoint: endpoint, bucket: "private-jobs", prefix: "new-api-async", credentials: aws.Credentials{AccessKeyID: "test-key", SecretAccessKey: "test-secret"}, client: &http.Client{Transport: server.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, signer: v4.NewSigner()}
	_, err = store.Put(t.Context(), "jobs/test/input", "text/plain", strings.NewReader("input"), 64)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "307")
	assert.NotContains(t, err.Error(), "secret-test")
	assert.NotContains(t, err.Error(), "127.0.0.1")
}

func TestAsyncArtifactDownloadRejectsPrivateAndCredentialBearingURLs(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/file", "http://169.254.169.254/latest/meta-data/", "http://[::1]/file", "http://10.0.0.1/file", "file:///etc/passwd", "https://user:secret@public.example/file"} {
		response, err := DownloadAsyncArtifact(t.Context(), target)
		require.Error(t, err)
		assert.Nil(t, response)
		assert.NotContains(t, err.Error(), "secret")
	}
}

func TestAsyncR2RecoveryObjectHEADRetainsStrictGETSizeCheck(t *testing.T) {
	payload := `{"version":1,"job_id":"async-recovery"}`
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 "))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		if r.Method == http.MethodGet {
			gets++
			_, _ = io.WriteString(w, payload)
		} else {
			assert.Equal(t, http.MethodHead, r.Method)
		}
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	store := &asyncR2Store{endpoint: endpoint, bucket: "private-jobs", prefix: "new-api-async", credentials: aws.Credentials{AccessKeyID: "test-key", SecretAccessKey: "test-secret"}, client: server.Client(), signer: v4.NewSigner()}
	previous := GetAsyncObjectStore()
	SetAsyncObjectStore(&trackedAsyncObjectStore{AsyncObjectStore: store})
	t.Cleanup(func() { SetAsyncObjectStore(previous) })
	body, mime, size, err := OpenAsyncRecoveryObject(t.Context(), "async-recovery/native-receipt", 128)
	require.NoError(t, err)
	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	assert.Equal(t, payload, string(raw))
	assert.Equal(t, "application/json", mime)
	assert.EqualValues(t, len(payload), size)
	_, _, _, err = OpenAsyncRecoveryObject(t.Context(), "async-recovery/native-receipt", 3)
	require.Error(t, err)
	assert.Equal(t, 1, gets, "oversized receipt must fail at HEAD before GET")
	_, _, _, err = store.Open(t.Context(), AsyncObjectRef{Backend: "r2", Key: "async-recovery/native-receipt", Size: 0})
	require.Error(t, err)
}
