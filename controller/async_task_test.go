package controller

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

type asyncAdmissionStore struct{ bodies map[string][]byte }

func (s *asyncAdmissionStore) Enabled() bool { return true }
func (s *asyncAdmissionStore) Put(_ context.Context, key, contentType string, r io.Reader, max int64) (service.AsyncObjectRef, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return service.AsyncObjectRef{}, err
	}
	s.bodies[key] = raw
	return service.AsyncObjectRef{Backend: "r2", Key: key, ContentType: contentType, Size: int64(len(raw))}, nil
}
func (s *asyncAdmissionStore) Open(context.Context, service.AsyncObjectRef) (io.ReadCloser, string, int64, error) {
	panic("admission must not read results")
}
func (s *asyncAdmissionStore) Delete(_ context.Context, ref service.AsyncObjectRef) error {
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
	for _, body := range []string{`{"endpoint":"/v1/images/generations","request":{"model":"image-model","prompt":"hello","n":1}}`, `{"request":{"n":1,"prompt":"hello","model":"image-model"},"endpoint":"/v1/images/generations"}`} {
		c, recorder := submitAsyncTestContext("application/json", []byte(body))
		SubmitAsyncTask(c)
		require.Equal(t, 202, recorder.Code)
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.AsyncJob{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	assert.Len(t, store.bodies, 1)
	c, recorder := submitAsyncTestContext("application/json", []byte(`{"endpoint":"/v1/images/generations","request":{"model":"image-model","prompt":"different"}}`))
	SubmitAsyncTask(c)
	assert.Equal(t, 409, recorder.Code)
	assert.Len(t, store.bodies, 1)
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

func asyncMultipartTestBody(t *testing.T, boundary string) ([]byte, string) {
	t.Helper()
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	require.NoError(t, writer.SetBoundary(boundary))
	for _, field := range [][2]string{{"endpoint", "/v1/images/edits"}, {"model", "image-model"}, {"n", "1"}, {"prompt", "edit"}} {
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
	for _, boundary := range []string{"transport-boundary-one", "transport-boundary-two"} {
		body, contentType := asyncMultipartTestBody(t, boundary)
		c, recorder := submitAsyncTestContext(contentType, body)
		SubmitAsyncTask(c)
		require.Equal(t, 202, recorder.Code, recorder.Body.String())
	}
	var jobs []model.AsyncJob
	require.NoError(t, model.DB.Find(&jobs).Error)
	require.Len(t, jobs, 1)
	assert.Equal(t, "image-model", jobs[0].Model)
	assert.Len(t, store.bodies, 1)
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
