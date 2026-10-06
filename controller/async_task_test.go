package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type asyncAdmissionStore struct {
	bodies        map[string][]byte
	puts, deletes int
}

func (s *asyncAdmissionStore) Enabled() bool { return true }
func (s *asyncAdmissionStore) Put(_ context.Context, key, contentType string, r io.Reader, max int64) (service.AsyncObjectRef, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return service.AsyncObjectRef{}, err
	}
	s.puts++
	s.bodies[key] = raw
	return service.AsyncObjectRef{Backend: "r2", Key: key, ContentType: contentType, Size: int64(len(raw))}, nil
}
func (s *asyncAdmissionStore) Open(context.Context, service.AsyncObjectRef) (io.ReadCloser, string, int64, error) {
	panic("admission must not read results")
}
func (s *asyncAdmissionStore) Delete(_ context.Context, ref service.AsyncObjectRef) error {
	s.deletes++
	delete(s.bodies, ref.Key)
	return nil
}

func asyncAdmissionFixture(t *testing.T) *asyncAdmissionStore {
	t.Helper()
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedis; common.BatchUpdateEnabled = previousBatch })
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.AsyncJob{}, &model.User{}))
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "async-admission"}).Error)
	model.DB = db
	store := &asyncAdmissionStore{bodies: map[string][]byte{}}
	previousStore := service.GetAsyncObjectStore()
	service.SetAsyncObjectStore(store)
	t.Cleanup(func() { model.DB = previousDB; service.SetAsyncObjectStore(previousStore); _ = sqlDB.Close() })
	return store
}

func submitAsyncTestContext(contentType string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/async/tasks", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)
	c.Request.Header.Set("Idempotency-Key", "operation-1")
	c.Set("id", 1)
	c.Set("token_id", 2)
	model.SetBusinessMetadata(c, model.BusinessMetadata{TagLevel1: "system-b", TagLevel2: "product"})
	return c, recorder
}

func TestAsyncAdmissionCanonicalJSONAndConflict(t *testing.T) {
	store := asyncAdmissionFixture(t)
	for _, body := range []string{`{"endpoint":"/v1/images/generations","request":{"model":"image-model","prompt":"hello","n":1}}`, `{"request":{"n":1,"prompt":"hello","model":"image-model"},"endpoint":"/v1/images/generations"}`, `{"endpoint":"/v1/images/generations","retention_seconds":604800,"request":{"model":"image-model","prompt":"hello","n":1}}`, `{"endpoint":"/v1/images/generations","retention_seconds":null,"request":{"model":"image-model","prompt":"hello","n":1}}`} {
		c, recorder := submitAsyncTestContext("application/json", []byte(body))
		SubmitAsyncTask(c)
		require.Equal(t, 202, recorder.Code)
		var response struct {
			RetentionSeconds int64 `json:"retention_seconds"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		assert.EqualValues(t, 604800, response.RetentionSeconds)
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.AsyncJob{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	assert.Len(t, store.bodies, 1)
	c, recorder := submitAsyncTestContext("application/json", []byte(`{"endpoint":"/v1/images/generations","request":{"model":"image-model","prompt":"different"}}`))
	SubmitAsyncTask(c)
	assert.Equal(t, 409, recorder.Code)
	assert.Len(t, store.bodies, 1)
	c, recorder = submitAsyncTestContext("application/json", []byte(`{"endpoint":"/v1/images/generations","retention_seconds":86400,"request":{"model":"image-model","prompt":"hello","n":1}}`))
	SubmitAsyncTask(c)
	assert.Equal(t, 409, recorder.Code, "changing the retention window changes the idempotent request")
	assert.Len(t, store.bodies, 1)
}

func TestAsyncAdmissionRetentionJSONValidationAndSnapshot(t *testing.T) {
	for _, test := range []struct {
		name     string
		field    string
		expected int64
	}{
		{"omitted", "", 604800},
		{"null", `,"retention_seconds":null`, 604800},
		{"minimum", `,"retention_seconds":600`, 600},
		{"custom", `,"retention_seconds":432000`, 432000},
		{"maximum", `,"retention_seconds":2592000`, 2592000},
		{"zero", `,"retention_seconds":0`, 0},
		{"below-minimum", `,"retention_seconds":599`, 0},
		{"old-minimum", `,"retention_seconds":60`, 0},
		{"above-maximum", `,"retention_seconds":2592001`, 0},
		{"negative", `,"retention_seconds":-1`, 0},
		{"fractional", `,"retention_seconds":600.5`, 0},
		{"numeric-string", `,"retention_seconds":"86400"`, 0},
		{"boolean", `,"retention_seconds":true`, 0},
		{"array", `,"retention_seconds":[]`, 0},
		{"integer-overflow", `,"retention_seconds":9223372036854775808`, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := asyncAdmissionFixture(t)
			body := fmt.Sprintf(`{"endpoint":"/v1/images/generations"%s,"request":{"model":"image-model","prompt":"hello","n":1}}`, test.field)
			c, recorder := submitAsyncTestContext("application/json", []byte(body))
			SubmitAsyncTask(c)
			var jobs []model.AsyncJob
			require.NoError(t, model.DB.Find(&jobs).Error)
			if test.expected == 0 {
				assert.Equal(t, 400, recorder.Code, recorder.Body.String())
				assert.Empty(t, store.bodies, "invalid retention must fail before R2 upload")
				assert.Empty(t, jobs, "invalid retention must not enter the queue")
				return
			}
			require.Equal(t, 202, recorder.Code, recorder.Body.String())
			require.Len(t, jobs, 1)
			assert.Equal(t, test.expected, jobs[0].RetentionSeconds)
			var response struct {
				RetentionSeconds int64 `json:"retention_seconds"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, test.expected, response.RetentionSeconds)
			require.Len(t, store.bodies, 1)
			for _, forwarded := range store.bodies {
				var request map[string]any
				require.NoError(t, common.Unmarshal(forwarded, &request))
				assert.NotContains(t, request, "retention_seconds", "retention is gateway metadata, not a provider option")
				assert.Equal(t, "hello", request["prompt"])
			}
			c, read := submitAsyncTestContext("application/json", nil)
			c.Params = gin.Params{{Key: "id", Value: jobs[0].JobID}}
			GetAsyncTask(c)
			require.Equal(t, 200, read.Code)
			require.NoError(t, common.Unmarshal(read.Body.Bytes(), &response))
			assert.Equal(t, test.expected, response.RetentionSeconds, "task retrieval uses its saved retention snapshot")
		})
	}
}

func TestAsyncAdmissionRejectsInvalidModelCountAndPolicyBeforeUpload(t *testing.T) {
	store := asyncAdmissionFixture(t)
	for _, body := range []string{`{"endpoint":"/v1/images/generations","request":{"n":1}}`, `{"endpoint":"/v1/images/generations","request":{"model":"image-model","n":1000000}}`, `{"endpoint":"/v1/tasks/arbitrary-plugin","request":{"model":"image-model"}}`, `{"endpoint":"/v1/images/generations","request":{"model":"image-model","stream":true}}`} {
		c, recorder := submitAsyncTestContext("application/json", []byte(body))
		SubmitAsyncTask(c)
		assert.Equal(t, 400, recorder.Code)
	}
	c, recorder := submitAsyncTestContext("application/json", []byte(`{"endpoint":"/v1/images/generations","request":{"model":"image-model"}}`))
	c.Set("token_model_limit_enabled", true)
	c.Set("token_model_limit", map[string]bool{"another-model": true})
	SubmitAsyncTask(c)
	assert.Equal(t, 403, recorder.Code)
	assert.Empty(t, store.bodies)
}

func TestAsyncAdmissionRejectsAudioSSEBeforeStorage(t *testing.T) {
	store := asyncAdmissionFixture(t)
	c, recorder := submitAsyncTestContext("application/json", []byte(`{"endpoint":"/v1/audio/speech","request":{"model":"tts-model","input":"hello","stream_format":"sse"}}`))
	SubmitAsyncTask(c)
	assert.Equal(t, 400, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "async_stream_not_supported")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, item := range [][2]string{{"endpoint", "/v1/audio/speech"}, {"model", "tts-model"}, {"input", "hello"}, {"stream_format", "sse"}} {
		require.NoError(t, writer.WriteField(item[0], item[1]))
	}
	require.NoError(t, writer.Close())
	c, recorder = submitAsyncTestContext(writer.FormDataContentType(), body.Bytes())
	SubmitAsyncTask(c)
	assert.Equal(t, 400, recorder.Code)
	assert.Empty(t, store.bodies)
	var count int64
	require.NoError(t, model.DB.Model(&model.AsyncJob{}).Count(&count).Error)
	assert.Zero(t, count)
}

func asyncMultipartTestBody(t *testing.T, boundary string, fields ...[2]string) ([]byte, string) {
	t.Helper()
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	require.NoError(t, writer.SetBoundary(boundary))
	for _, field := range append([][2]string{{"endpoint", "/v1/images/edits"}, {"model", "image-model"}, {"n", "1"}, {"prompt", "edit"}}, fields...) {
		require.NoError(t, writer.WriteField(field[0], field[1]))
	}
	part, err := writer.CreateFormFile("image[]", "example.png")
	require.NoError(t, err)
	_, err = io.Copy(part, strings.NewReader("image-content"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return raw.Bytes(), writer.FormDataContentType()
}

func TestAsyncAdmissionMultipartBoundaryIndependentIdempotency(t *testing.T) {
	store := asyncAdmissionFixture(t)
	for _, test := range []struct {
		boundary string
		fields   [][2]string
	}{{"transport-boundary-one", nil}, {"transport-boundary-two", [][2]string{{"retention_seconds", "604800"}}}} {
		body, contentType := asyncMultipartTestBody(t, test.boundary, test.fields...)
		c, recorder := submitAsyncTestContext(contentType, body)
		SubmitAsyncTask(c)
		require.Equal(t, 202, recorder.Code, recorder.Body.String())
	}
	var jobs []model.AsyncJob
	require.NoError(t, model.DB.Find(&jobs).Error)
	require.Len(t, jobs, 1)
	assert.Equal(t, "image-model", jobs[0].Model)
	assert.EqualValues(t, 604800, jobs[0].RetentionSeconds)
	assert.Len(t, store.bodies, 1)
	for _, raw := range store.bodies {
		_, params, err := mime.ParseMediaType(jobs[0].ContentType)
		require.NoError(t, err)
		reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			assert.NotEqual(t, "retention_seconds", part.FormName(), "gateway retention must be stripped from provider form fields")
		}
	}
	body, contentType := asyncMultipartTestBody(t, "changed-retention-boundary", [2]string{"retention_seconds", "86400"})
	c, recorder := submitAsyncTestContext(contentType, body)
	SubmitAsyncTask(c)
	assert.Equal(t, 409, recorder.Code, "multipart idempotency includes normalized retention")
	assert.Len(t, store.bodies, 1)
}

func TestAsyncAdmissionRetentionMultipartValidation(t *testing.T) {
	for _, test := range []struct {
		name     string
		fields   [][2]string
		expected int64
	}{
		{"minimum", [][2]string{{"retention_seconds", "600"}}, 600},
		{"custom", [][2]string{{"retention_seconds", "86400"}}, 86400},
		{"maximum", [][2]string{{"retention_seconds", "2592000"}}, 2592000},
		{"empty", [][2]string{{"retention_seconds", ""}}, 0},
		{"zero", [][2]string{{"retention_seconds", "0"}}, 0},
		{"below-minimum", [][2]string{{"retention_seconds", "599"}}, 0},
		{"old-minimum", [][2]string{{"retention_seconds", "60"}}, 0},
		{"above-maximum", [][2]string{{"retention_seconds", "2592001"}}, 0},
		{"fractional", [][2]string{{"retention_seconds", "600.5"}}, 0},
		{"integer-overflow", [][2]string{{"retention_seconds", "9223372036854775808"}}, 0},
		{"duplicate", [][2]string{{"retention_seconds", "86400"}, {"retention_seconds", "86400"}}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := asyncAdmissionFixture(t)
			body, contentType := asyncMultipartTestBody(t, "retention-validation", test.fields...)
			c, recorder := submitAsyncTestContext(contentType, body)
			SubmitAsyncTask(c)
			var jobs []model.AsyncJob
			require.NoError(t, model.DB.Find(&jobs).Error)
			if test.expected == 0 {
				assert.Equal(t, 400, recorder.Code, recorder.Body.String())
				assert.Empty(t, store.bodies)
				assert.Empty(t, jobs)
				return
			}
			require.Equal(t, 202, recorder.Code, recorder.Body.String())
			require.Len(t, jobs, 1)
			assert.Equal(t, test.expected, jobs[0].RetentionSeconds)
		})
	}
	t.Run("file-instead-of-text", func(t *testing.T) {
		store := asyncAdmissionFixture(t)
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("endpoint", "/v1/images/generations"))
		require.NoError(t, writer.WriteField("model", "image-model"))
		part, err := writer.CreateFormFile("retention_seconds", "retention.txt")
		require.NoError(t, err)
		_, err = io.WriteString(part, "86400")
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		c, recorder := submitAsyncTestContext(writer.FormDataContentType(), body.Bytes())
		SubmitAsyncTask(c)
		assert.Equal(t, 400, recorder.Code)
		assert.Empty(t, store.bodies)
		var count int64
		require.NoError(t, model.DB.Model(&model.AsyncJob{}).Count(&count).Error)
		assert.Zero(t, count)
	})
}

func TestAsyncAdmissionLegacyRetentionReplayPreservesOriginalWindow(t *testing.T) {
	for _, format := range []string{"json", "multipart"} {
		t.Run(format, func(t *testing.T) {
			store := asyncAdmissionFixture(t)
			endpoint, contentType := "/v1/images/generations", "application/json"
			forwarded, err := common.Marshal(map[string]any{"model": "image-model", "prompt": "hello", "n": 1})
			require.NoError(t, err)
			if format == "multipart" {
				endpoint = "/v1/images/edits"
				body, originalType := asyncMultipartTestBody(t, "legacy-original-boundary")
				_, params, err := mime.ParseMediaType(originalType)
				require.NoError(t, err)
				_, forwarded, contentType, err = parseAsyncMultipart(body, params["boundary"])
				require.NoError(t, err)
			}
			c, _ := submitAsyncTestContext(contentType, nil)
			metadata := model.BusinessMetadataFromContext(c)
			scopeBytes, err := common.Marshal(model.BusinessScopeFromContext(c))
			require.NoError(t, err)
			scopeHash := sha256.Sum256(scopeBytes)
			keyHash := sha256.Sum256([]byte("operation-1"))
			bodyHash := sha256.Sum256(forwarded)
			legacyBytes, err := common.Marshal([]any{endpoint, "image", hex.EncodeToString(bodyHash[:]), metadata.BusinessID, metadata.ExternalUserID, metadata.ExternalTaskID})
			require.NoError(t, err)
			legacyHash := sha256.Sum256(legacyBytes)
			now := time.Now().Unix()
			job := &model.AsyncJob{JobID: "async-legacy-retention", UserID: 1, TokenID: 2, BusinessMetadata: metadata,
				IdempotencyScope: hex.EncodeToString(scopeHash[:]), IdempotencyKey: hex.EncodeToString(keyHash[:]), Fingerprint: hex.EncodeToString(legacyHash[:]),
				Endpoint: endpoint, Modality: "image", Model: "image-model", ContentType: contentType, Status: model.AsyncJobCompleted,
				CreatedAt: now - 1000, CompletedAt: now - 700, ResultExpiresAt: now - 700 + 86400, SummaryExpiresAt: now + 30*24*60*60}
			require.NoError(t, model.DB.Create(job).Error)
			retentions := []string{"", "86400", "604800", "600"}
			if format == "json" {
				retentions = append(retentions, "null")
			}
			for index, retention := range retentions {
				var body []byte
				if format == "multipart" {
					var fields [][2]string
					if retention != "" {
						fields = append(fields, [2]string{"retention_seconds", retention})
					}
					body, contentType = asyncMultipartTestBody(t, "legacy-replay-"+strconv.Itoa(index), fields...)
				} else {
					field := ""
					if retention != "" {
						field = `,"retention_seconds":` + retention
					}
					body = fmt.Appendf(nil, `{"endpoint":"%s"%s,"request":%s}`, endpoint, field, forwarded)
				}
				c, recorder := submitAsyncTestContext(contentType, body)
				SubmitAsyncTask(c)
				if retention != "" && retention != "86400" && retention != "null" {
					assert.Equal(t, 409, recorder.Code, "legacy tasks cannot change their original retention window on replay")
					continue
				}
				require.Equal(t, 202, recorder.Code, recorder.Body.String())
				var response struct {
					ID               string `json:"id"`
					RetentionSeconds int64  `json:"retention_seconds"`
					ResultExpiresAt  int64  `json:"result_expires_at"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				assert.Equal(t, job.JobID, response.ID)
				assert.EqualValues(t, 86400, response.RetentionSeconds)
				assert.Equal(t, job.ResultExpiresAt, response.ResultExpiresAt)
			}
			c, recorder := submitAsyncTestContext("application/json", nil)
			c.Params = gin.Params{{Key: "id", Value: job.JobID}}
			GetAsyncTask(c)
			require.Equal(t, 200, recorder.Code)
			var fetched model.AsyncJob
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &fetched))
			assert.EqualValues(t, 86400, fetched.RetentionSeconds)
			var unchanged model.AsyncJob
			require.NoError(t, model.DB.First(&unchanged, job.ID).Error)
			assert.Zero(t, unchanged.RetentionSeconds, "legacy response normalization does not migrate its retention snapshot")
			assert.Equal(t, job.ResultExpiresAt, unchanged.ResultExpiresAt)
			assert.Equal(t, job.CompletedAt, unchanged.CompletedAt)
			assert.Equal(t, job.CreatedAt, unchanged.CreatedAt)
			assert.Equal(t, job.SummaryExpiresAt, unchanged.SummaryExpiresAt)
			assert.Equal(t, job.Fingerprint, unchanged.Fingerprint)
			assert.Empty(t, store.bodies, "legacy replay must not upload or submit another generation request")
			var count int64
			require.NoError(t, model.DB.Model(&model.AsyncJob{}).Count(&count).Error)
			assert.EqualValues(t, 1, count)
		})
	}
}

func TestAsyncAdmissionLegacyRetentionConcurrentReplayCleansOrphan(t *testing.T) {
	for _, test := range []struct {
		name     string
		field    string
		expected int
	}{
		{"omitted", "", http.StatusAccepted},
		{"explicit-legacy-window", `,"retention_seconds":86400`, http.StatusAccepted},
		{"different-window", `,"retention_seconds":604800`, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := asyncAdmissionFixture(t)
			forwarded, err := common.Marshal(map[string]any{"model": "image-model", "prompt": "hello", "n": 1})
			require.NoError(t, err)
			body := fmt.Appendf(nil, `{"endpoint":"/v1/images/generations"%s,"request":%s}`, test.field, forwarded)
			c, recorder := submitAsyncTestContext("application/json", body)
			metadata := model.BusinessMetadataFromContext(c)
			scopeBytes, err := common.Marshal(model.BusinessScopeFromContext(c))
			require.NoError(t, err)
			scopeHash := sha256.Sum256(scopeBytes)
			keyHash := sha256.Sum256([]byte("operation-1"))
			bodyHash := sha256.Sum256(forwarded)
			legacyBytes, err := common.Marshal([]any{"/v1/images/generations", "image", hex.EncodeToString(bodyHash[:]), metadata.BusinessID, metadata.ExternalUserID, metadata.ExternalTaskID})
			require.NoError(t, err)
			legacyHash := sha256.Sum256(legacyBytes)
			now := time.Now().Unix()
			legacy := &model.AsyncJob{JobID: "async-legacy-race", UserID: 1, TokenID: 2, BusinessMetadata: metadata,
				IdempotencyScope: hex.EncodeToString(scopeHash[:]), IdempotencyKey: hex.EncodeToString(keyHash[:]), Fingerprint: hex.EncodeToString(legacyHash[:]),
				Endpoint: "/v1/images/generations", Modality: "image", Model: "image-model", Status: model.AsyncJobCompleted,
				CreatedAt: now - 1000, CompletedAt: now - 700, ResultExpiresAt: now - 700 + 86400, SummaryExpiresAt: now + 30*24*60*60}
			injected := false
			var insertErr error
			const callback = "test:async_legacy_admission_race"
			require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if injected || tx.Statement.Table != "async_jobs" || !errors.Is(tx.Error, gorm.ErrRecordNotFound) {
					return
				}
				// The fast lookup really misses. An old instance commits its
				// legacy row before this instance reaches transactional admission.
				// Set the guard before Create, which does not invoke Query callbacks.
				injected = true
				insertErr = model.DB.Create(legacy).Error
			}))
			t.Cleanup(func() { _ = model.DB.Callback().Query().Remove(callback) })
			SubmitAsyncTask(c)
			require.True(t, injected)
			require.NoError(t, insertErr)
			require.Equal(t, test.expected, recorder.Code, recorder.Body.String())
			if test.expected == http.StatusAccepted {
				var response struct {
					ID               string `json:"id"`
					RetentionSeconds int64  `json:"retention_seconds"`
					ResultExpiresAt  int64  `json:"result_expires_at"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				assert.Equal(t, legacy.JobID, response.ID)
				assert.EqualValues(t, 86400, response.RetentionSeconds)
				assert.Equal(t, legacy.ResultExpiresAt, response.ResultExpiresAt)
				assert.Equal(t, "/v1/async/tasks/"+legacy.JobID, recorder.Header().Get("Location"))
			}
			var jobs []model.AsyncJob
			require.NoError(t, model.DB.Find(&jobs).Error)
			require.Len(t, jobs, 1, "the raced replay must not enqueue another generation")
			assert.Equal(t, legacy.JobID, jobs[0].JobID)
			assert.Zero(t, jobs[0].RetentionSeconds)
			assert.Equal(t, legacy.Fingerprint, jobs[0].Fingerprint)
			assert.Equal(t, legacy.CompletedAt, jobs[0].CompletedAt)
			assert.Equal(t, legacy.ResultExpiresAt, jobs[0].ResultExpiresAt)
			assert.Equal(t, legacy.SummaryExpiresAt, jobs[0].SummaryExpiresAt)
			assert.Equal(t, 1, store.puts, "the racing path uploads before transactional admission detects the old row")
			assert.Equal(t, 1, store.deletes, "the losing request upload must be deleted")
			assert.Empty(t, store.bodies)
		})
	}
}

func TestAsyncAdmissionPendingLimitAndBillingConfigurationGuard(t *testing.T) {
	store := asyncAdmissionFixture(t)
	require.NoError(t, model.SetAsyncPendingPerAccountLimit(1))
	t.Cleanup(func() { _ = model.SetAsyncPendingPerAccountLimit(100) })
	body := []byte(`{"endpoint":"/v1/images/generations","request":{"model":"image-model"}}`)
	c, recorder := submitAsyncTestContext("application/json", body)
	SubmitAsyncTask(c)
	require.Equal(t, 202, recorder.Code)
	c, recorder = submitAsyncTestContext("application/json", body)
	SubmitAsyncTask(c)
	require.Equal(t, 202, recorder.Code)
	c, recorder = submitAsyncTestContext("application/json", body)
	c.Request.Header.Set("Idempotency-Key", "new-operation")
	SubmitAsyncTask(c)
	assert.Equal(t, 429, recorder.Code)
	assert.Len(t, store.bodies, 1)
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	t.Cleanup(func() { common.RedisEnabled = previousRedis; common.BatchUpdateEnabled = previousBatch })
	for _, test := range []struct{ redis, batch bool }{{true, false}, {false, true}} {
		common.RedisEnabled = test.redis
		common.BatchUpdateEnabled = test.batch
		c, recorder = submitAsyncTestContext("application/json", body)
		SubmitAsyncTask(c)
		assert.Equal(t, 503, recorder.Code)
		assert.Contains(t, recorder.Body.String(), "unsupported_billing_configuration")
	}
}

func TestAsyncTaskReadExactBusinessDomainAndManagementOwner(t *testing.T) {
	asyncAdmissionFixture(t)
	job := &model.AsyncJob{JobID: "async-readable", UserID: 1, BusinessMetadata: model.BusinessMetadata{TagLevel1: "System-B", TagLevel2: "product"}, IdempotencyScope: "read-scope", IdempotencyKey: "read-key", Fingerprint: "read-body", Status: model.AsyncJobCompleted, SummaryExpiresAt: time.Now().Unix() + 3600, ResultExpiresAt: time.Now().Unix() - 1}
	require.NoError(t, model.DB.Create(job).Error)
	for _, test := range []struct {
		user, token int
		tag         string
		expected    int
	}{{1, 2, "system-b", 404}, {2, 0, "", 404}, {1, 0, "", 200}, {1, 2, "System-B", 200}} {
		c, recorder := submitAsyncTestContext("application/json", nil)
		c.Set("id", test.user)
		c.Set("token_id", test.token)
		model.SetBusinessMetadata(c, model.BusinessMetadata{TagLevel1: test.tag, TagLevel2: "product"})
		c.Params = gin.Params{{Key: "id", Value: job.JobID}}
		GetAsyncTask(c)
		assert.Equal(t, test.expected, recorder.Code)
	}
	c, recorder := submitAsyncTestContext("application/json", nil)
	c.Set("token_id", 0)
	c.Params = gin.Params{{Key: "id", Value: job.JobID}}
	GetAsyncTaskResult(c)
	assert.Equal(t, 410, recorder.Code)
	encoded, err := common.Marshal(job)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "read-key")
	assert.NotContains(t, string(encoded), "read-body")
}
