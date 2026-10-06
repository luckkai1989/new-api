package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const asyncRequestMaxBytes int64 = 64 << 20

type asyncTaskRequest struct {
	Endpoint         string          `json:"endpoint"`
	Request          json.RawMessage `json:"request"`
	BusinessID       string          `json:"business_id,omitempty"`
	ExternalUserID   string          `json:"external_user_id,omitempty"`
	ExternalTaskID   string          `json:"external_task_id,omitempty"`
	TaskID           string          `json:"task_id,omitempty"`
	RetentionSeconds *int64          `json:"retention_seconds,omitempty"`
}

// Only an in-process worker can install the typed execution context.
func AsyncExecutionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if job := service.AsyncJobFromExecutionContext(c.Request.Context()); job != nil {
			c.Set("async_job_id", job.JobID)
			c.Set(common.RequestIdKey, job.JobID)
		}
		c.Next()
	}
}

func SubmitAsyncTask(c *gin.Context) {
	store := service.GetAsyncObjectStore()
	if code, message := service.AsyncTaskUnavailableReason(); code != "" {
		asyncTaskError(c, 503, code, message)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, asyncRequestMaxBytes+1))
	if err != nil || int64(len(raw)) > asyncRequestMaxBytes {
		asyncTaskError(c, 413, "request_too_large", "Async request exceeds 64 MiB")
		return
	}
	var input asyncTaskRequest
	var forwarded []byte
	contentType := "application/json"
	mediaType, params, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if mediaType == "multipart/form-data" {
		input, forwarded, contentType, err = parseAsyncMultipart(raw, params["boundary"])
	} else {
		err = common.Unmarshal(raw, &input)
		forwarded = input.Request
	}
	if err != nil || len(forwarded) == 0 {
		asyncTaskError(c, 400, "invalid_async_request", "Provide endpoint and request, or multipart generation fields")
		return
	}
	retentionSeconds := model.AsyncDefaultRetentionSeconds
	if input.RetentionSeconds != nil {
		retentionSeconds = *input.RetentionSeconds
	}
	if retentionSeconds < model.AsyncMinRetentionSeconds || retentionSeconds > model.AsyncMaxRetentionSeconds {
		asyncTaskError(c, 400, "invalid_retention_seconds", "retention_seconds must be an integer between 600 and 2592000 seconds")
		return
	}
	modality := asyncEndpointModality(input.Endpoint)
	if modality == "" {
		asyncTaskError(c, 400, "unsupported_async_endpoint", "Only supported image, video and audio endpoints can be asynchronous")
		return
	}
	if err := middleware.CheckBusinessModality(c, modality); err != nil {
		asyncTaskError(c, 403, "modality_not_allowed", err.Error())
		return
	}
	modelName := ""
	if mediaType != "multipart/form-data" {
		var request map[string]any
		if common.Unmarshal(forwarded, &request) != nil || request == nil {
			asyncTaskError(c, 400, "invalid_async_request", "request must be a JSON object")
			return
		}
		modelName, _ = request["model"].(string)
		if stream, _ := request["stream"].(bool); stream {
			asyncTaskError(c, 400, "async_stream_not_supported", "Use a non-streaming media request")
			return
		}
		if streamFormat, _ := request["stream_format"].(string); strings.EqualFold(streamFormat, "sse") {
			asyncTaskError(c, 400, "async_stream_not_supported", "Use a non-streaming media request")
			return
		}
		forwarded, err = common.Marshal(request)
		if err != nil {
			asyncTaskError(c, 400, "invalid_async_request", "Invalid request")
			return
		}
	}
	modelName, err = validateAsyncMediaRequest(input.Endpoint, contentType, forwarded, modelName)
	if err != nil {
		asyncTaskError(c, 400, "invalid_generation_request", err.Error())
		return
	}
	if err := validateAsyncDurableInputs(input.Endpoint, contentType, forwarded); err != nil {
		asyncTaskError(c, 400, "external_async_input_not_supported", err.Error())
		return
	}
	if c.GetBool("token_model_limit_enabled") {
		value, _ := c.Get("token_model_limit")
		limits, _ := value.(map[string]bool)
		if !middleware.TokenModelLimitAllows(limits, modelName) {
			asyncTaskError(c, 403, "model_not_allowed", "API key cannot use this model")
			return
		}
	}
	metadata := model.BusinessMetadataFromContext(c)
	if input.ExternalTaskID == "" {
		input.ExternalTaskID = input.TaskID
	}
	for _, field := range []struct {
		value  string
		target *string
	}{{input.BusinessID, &metadata.BusinessID}, {input.ExternalUserID, &metadata.ExternalUserID}, {input.ExternalTaskID, &metadata.ExternalTaskID}} {
		if field.value != "" {
			normalized, err := model.NormalizeBusinessIdentifier(field.value, 128)
			if err != nil {
				asyncTaskError(c, 400, "invalid_business_id", "Invalid business attribution")
				return
			}
			*field.target = normalized
		}
	}
	id := "async_" + strings.TrimPrefix(model.GenerateTaskID(), "task_")
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		key = id
	}
	if len(key) > 128 || strings.ContainsAny(key, "\r\n\x00") {
		asyncTaskError(c, 400, "invalid_idempotency_key", "Idempotency-Key must be at most 128 bytes")
		return
	}
	scopeBytes, _ := common.Marshal(model.BusinessScopeFromContext(c))
	scopeHash := sha256.Sum256(scopeBytes)
	keyHash := sha256.Sum256([]byte(key))
	bodyHash := sha256.Sum256(forwarded)
	fingerprintFields := []any{input.Endpoint, modality, hex.EncodeToString(bodyHash[:]), metadata.BusinessID, metadata.ExternalUserID, metadata.ExternalTaskID}
	legacyFingerprintBytes, _ := common.Marshal(fingerprintFields)
	legacyFingerprint := sha256.Sum256(legacyFingerprintBytes)
	legacyFingerprintID := hex.EncodeToString(legacyFingerprint[:])
	fingerprintBytes, _ := common.Marshal(append(fingerprintFields, retentionSeconds))
	fingerprint := sha256.Sum256(fingerprintBytes)
	scopeID, keyID, fingerprintID := hex.EncodeToString(scopeHash[:]), hex.EncodeToString(keyHash[:]), hex.EncodeToString(fingerprint[:])
	if existing, lookupErr := model.GetAsyncJobByIdempotency(c.Request.Context(), scopeID, keyID); lookupErr == nil {
		if !asyncTaskRequestMatchesJob(existing, input.RetentionSeconds, fingerprintID, legacyFingerprintID) {
			asyncTaskError(c, 409, "idempotency_conflict", "Idempotency-Key already identifies a different request")
			return
		}
		existing.RetentionSeconds = existing.EffectiveRetentionSeconds()
		c.Header("Location", "/v1/async/tasks/"+existing.JobID)
		c.JSON(202, existing)
		return
	} else if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		asyncTaskError(c, 503, "async_admission_failed", "Task admission is temporarily unavailable")
		return
	}
	if pending, err := model.AsyncPendingCount(c.Request.Context(), c.GetInt("id")); err != nil {
		asyncTaskError(c, 503, "async_admission_failed", "Task admission is temporarily unavailable")
		return
	} else if pending >= model.AsyncPendingPerAccountLimit() {
		asyncTaskError(c, 429, "async_pending_limit", "Account has too many pending asynchronous tasks")
		return
	}
	ref, err := store.Put(c.Request.Context(), id+"/request", contentType, bytes.NewReader(forwarded), asyncRequestMaxBytes)
	if err != nil {
		asyncTaskError(c, 503, "request_storage_failed", "Could not durably save the request; no generation was started")
		return
	}
	refBytes, _ := common.Marshal(ref)
	now := time.Now().Unix()
	metadata.AsyncTaskID = id
	job := &model.AsyncJob{JobID: id, UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"), BusinessMetadata: metadata, IdempotencyScope: hex.EncodeToString(scopeHash[:]), IdempotencyKey: hex.EncodeToString(keyHash[:]), Fingerprint: hex.EncodeToString(fingerprint[:]), Endpoint: input.Endpoint, Modality: modality, Model: modelName, ContentType: contentType, ClientIP: c.ClientIP(), RequestRef: string(refBytes), RetentionSeconds: retentionSeconds, Status: model.AsyncJobQueued, CreatedAt: now, NextRunAt: now, SummaryExpiresAt: now + 30*24*60*60}
	persisted, created, err := model.CreateAsyncJob(c.Request.Context(), job)
	if err != nil || !created {
		_ = store.Delete(c.Request.Context(), ref)
	}
	if errors.Is(err, model.ErrAsyncJobConflict) {
		// During a rolling upgrade, an old instance can accept the same request
		// after our optimistic lookup. Reconcile it using the same replay policy.
		existing, lookupErr := model.GetAsyncJobByIdempotency(c.Request.Context(), scopeID, keyID)
		if lookupErr != nil {
			asyncTaskError(c, 503, "async_admission_failed", "Task admission is temporarily unavailable")
			return
		}
		if asyncTaskRequestMatchesJob(existing, input.RetentionSeconds, fingerprintID, legacyFingerprintID) {
			existing.RetentionSeconds = existing.EffectiveRetentionSeconds()
			c.Header("Location", "/v1/async/tasks/"+existing.JobID)
			c.JSON(http.StatusAccepted, existing)
			return
		}
		asyncTaskError(c, 409, "idempotency_conflict", "Idempotency-Key already identifies a different request")
		return
	}
	if errors.Is(err, model.ErrAsyncPendingLimit) {
		asyncTaskError(c, 429, "async_pending_limit", "Account has too many pending asynchronous tasks")
		return
	}
	if err != nil {
		asyncTaskError(c, 503, "async_admission_failed", "Could not durably queue the task; no generation was started")
		return
	}
	c.Header("Location", "/v1/async/tasks/"+persisted.JobID)
	c.JSON(http.StatusAccepted, persisted)
}

func asyncTaskRequestMatchesJob(job *model.AsyncJob, requestedRetention *int64, fingerprintID, legacyFingerprintID string) bool {
	if job.Fingerprint == fingerprintID {
		return true
	}
	// Old tasks have no retention snapshot or retention-aware fingerprint.
	// Replays keep their original one-day window, never silently extend it.
	return job.RetentionSeconds == 0 && (requestedRetention == nil || *requestedRetention == model.AsyncLegacyRetentionSeconds) && job.Fingerprint == legacyFingerprintID
}

func asyncEndpointModality(endpoint string) string {
	switch endpoint {
	case "/v1/images/generations", "/v1/images/edits":
		return "image"
	case "/v1/videos":
		return "video"
	case "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/audio/translations":
		return "audio"
	}
	return ""
}

func parseAsyncMultipart(raw []byte, boundary string) (asyncTaskRequest, []byte, string, error) {
	var input asyncTaskRequest
	var output bytes.Buffer
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	type storedPart struct {
		Header map[string][]string
		Value  []byte
	}
	parts := []storedPart{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return input, nil, "", err
		}
		value, err := io.ReadAll(part)
		if err != nil {
			return input, nil, "", err
		}
		name := part.FormName()
		if name == "retention_seconds" {
			if part.FileName() != "" || input.RetentionSeconds != nil {
				return input, nil, "", errors.New("retention_seconds must be a single integer text field")
			}
			seconds, err := strconv.ParseInt(string(value), 10, 64)
			if err != nil {
				return input, nil, "", errors.New("retention_seconds must be an integer text field")
			}
			input.RetentionSeconds = &seconds
			continue
		}
		if part.FileName() == "" {
			switch name {
			case "endpoint":
				input.Endpoint = string(value)
				continue
			case "business_id":
				input.BusinessID = string(value)
				continue
			case "external_user_id":
				input.ExternalUserID = string(value)
				continue
			case "external_task_id", "task_id":
				input.ExternalTaskID = string(value)
				continue
			case "stream":
				if string(value) == "true" {
					return input, nil, "", errors.New("stream not supported")
				}
			case "stream_format":
				if strings.EqualFold(string(value), "sse") {
					return input, nil, "", errors.New("stream not supported")
				}
			}
		}
		parts = append(parts, storedPart{Header: part.Header, Value: value})
	}
	// Multipart boundaries are transport details, not the idempotent request.
	// A deterministic boundary preserves the order of repeated image/file parts.
	canonical, err := common.Marshal(parts)
	if err != nil {
		return input, nil, "", err
	}
	digest := sha256.Sum256(canonical)
	writer := multipart.NewWriter(&output)
	if err = writer.SetBoundary(hex.EncodeToString(digest[:24])); err != nil {
		return input, nil, "", err
	}
	for _, part := range parts {
		target, err := writer.CreatePart(part.Header)
		if err != nil {
			return input, nil, "", err
		}
		if _, err = target.Write(part.Value); err != nil {
			return input, nil, "", err
		}
	}
	err = writer.Close()
	return input, output.Bytes(), writer.FormDataContentType(), err
}

func validateAsyncMediaRequest(endpoint, contentType string, body []byte, modelName string) (string, error) {
	request, err := http.NewRequest(http.MethodPost, "http://async.internal"+endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", contentType)
	validation := &gin.Context{Request: request, Keys: map[string]any{}}
	defer func() {
		if storage, err := common.GetBodyStorage(validation); err == nil {
			_ = storage.Close()
		}
		if request.MultipartForm != nil {
			_ = request.MultipartForm.RemoveAll()
		}
	}()
	modality := asyncEndpointModality(endpoint)
	if modality == "image" {
		parsed, err := helper.GetAndValidateRequest(validation, types.RelayFormatOpenAIImage)
		if err != nil {
			return "", err
		}
		modelName = parsed.(*dto.ImageRequest).Model
	} else if strings.HasPrefix(contentType, "multipart/form-data") {
		form, err := common.ParseMultipartFormReusable(validation)
		if err != nil {
			return "", err
		}
		request.MultipartForm = form
		if values := form.Value["model"]; len(values) > 0 {
			modelName = values[0]
		}
	} else if modality == "audio" {
		parsed, err := helper.GetAndValidateRequest(validation, types.RelayFormatOpenAIAudio)
		if err != nil {
			return "", err
		}
		modelName = parsed.(*dto.AudioRequest).Model
	}
	if strings.TrimSpace(modelName) == "" || len(modelName) > 191 || strings.ContainsAny(modelName, "\x00\r\n") {
		return "", errors.New("model is required and must be at most 191 bytes")
	}
	return modelName, nil
}

func asyncTaskForRead(c *gin.Context) (*model.AsyncJob, bool) {
	job, err := model.GetAsyncJob(c.Request.Context(), c.Param("id"))
	scope := model.BusinessScopeFromContext(c)
	if err != nil || job.UserID != scope.UserID || !model.BusinessScopeAccessible(c, job.UserID, job.BusinessMetadata) {
		asyncTaskError(c, 404, "task_not_found", "Task not found")
		return nil, false
	}
	if job.SummaryExpiresAt > 0 && time.Now().Unix() >= job.SummaryExpiresAt {
		asyncTaskError(c, 404, "task_not_found", "Task not found")
		return nil, false
	}
	job.RetentionSeconds = job.EffectiveRetentionSeconds()
	c.Header("Cache-Control", "private, no-store")
	return job, true
}

func GetAsyncTask(c *gin.Context) {
	job, ok := asyncTaskForRead(c)
	if ok {
		c.JSON(200, job)
	}
}
func GetAsyncTaskResult(c *gin.Context) {
	job, ok := asyncTaskForRead(c)
	if !ok || !asyncResultReadable(c, job) {
		return
	}
	var ref model.AsyncObjectRef
	if common.UnmarshalJsonStr(job.ResultRef, &ref) != nil {
		asyncTaskError(c, 503, "result_unavailable", "Result is not available")
		return
	}
	serveAsyncObject(c, ref)
}
func GetAsyncTaskArtifact(c *gin.Context) {
	job, ok := asyncTaskForRead(c)
	if !ok || !asyncResultReadable(c, job) {
		return
	}
	var refs map[string]model.AsyncObjectRef
	if common.UnmarshalJsonStr(job.ArtifactRefs, &refs) != nil {
		asyncTaskError(c, 404, "artifact_not_found", "Artifact not found")
		return
	}
	ref, exists := refs[c.Param("artifact_id")]
	if !exists {
		asyncTaskError(c, 404, "artifact_not_found", "Artifact not found")
		return
	}
	serveAsyncObject(c, ref)
}
func asyncResultReadable(c *gin.Context, job *model.AsyncJob) bool {
	if job.ResultExpiresAt > 0 && time.Now().Unix() >= job.ResultExpiresAt {
		asyncTaskError(c, 410, "result_expired", "Task result retention period has expired")
		return false
	}
	if job.Status != model.AsyncJobCompleted {
		asyncTaskError(c, 409, "result_not_ready", "Task has no completed result")
		return false
	}
	return true
}
func serveAsyncObject(c *gin.Context, ref model.AsyncObjectRef) {
	body, contentType, size, err := service.GetAsyncObjectStore().Open(c.Request.Context(), ref)
	if err != nil {
		asyncTaskError(c, 503, "result_unavailable", "Stored result is temporarily unavailable")
		return
	}
	defer body.Close()
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", "attachment")
	c.DataFromReader(200, size, contentType, body, nil)
}
func asyncTaskError(c *gin.Context, status int, code, message string) {
	c.Header("Cache-Control", "private, no-store")
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message, "type": code}})
}
