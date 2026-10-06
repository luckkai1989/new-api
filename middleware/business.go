package middleware

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Existing public MJ image URLs remain usable after upgrade. This exception
// cannot apply to newly created empty-label tasks or any business snapshot.
func MidjourneyImageReadAuth() gin.HandlerFunc {
	readAuth := TokenOrUserReadAuth()
	return func(c *gin.Context) {
		task := model.GetByOnlyMJId(c.Param("id"))
		if task == nil {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "midjourney_task_not_found"})
			return
		}
		if task.IsLegacyPublicImage() {
			c.Next()
			return
		}
		readAuth(c)
	}
}

func setupBusinessTokenContext(c *gin.Context, token *model.Token) error {
	modalities, err := token.GetAllowedModalities()
	if err != nil {
		abortWithOpenAiMessage(c, http.StatusForbidden, "Invalid token modality policy")
		return err
	}
	c.Set(model.BusinessModalitiesContextKey, modalities)
	if job := service.AsyncJobFromExecutionContext(c.Request.Context()); job != nil {
		if job.UserID != token.UserId || job.TokenID != token.Id || job.JobID == "" {
			abortWithOpenAiMessage(c, http.StatusForbidden, "Invalid async execution identity")
			return errors.New("invalid async execution identity")
		}
		metadata := job.BusinessMetadata
		metadata.AsyncTaskID = job.JobID
		model.SetBusinessMetadata(c, metadata)
		model.SetInternalAsyncTaskAccess(c, job.JobID)
		return nil
	}
	metadata := model.BusinessMetadata{BusinessSystemID: c.GetString("business_system_id"), TagLevel1: token.TagLevel1, TagLevel2: token.TagLevel2}
	for _, field := range []struct {
		header string
		target *string
	}{{"X-Business-ID", &metadata.BusinessID}, {"X-External-User-ID", &metadata.ExternalUserID}, {"X-External-Task-ID", &metadata.ExternalTaskID}} {
		value, err := model.NormalizeBusinessIdentifier(c.GetHeader(field.header), 128)
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, "Invalid business attribution header")
			return err
		}
		*field.target = value
	}
	model.SetBusinessMetadata(c, metadata)
	return nil
}

// Internal archival reads use the already-admitted job identity, never an
// HTTP credential exception. Revocation stops new submissions immediately,
// without destroying already-generated files before they can be archived.
func setupInternalAsyncReadContext(c *gin.Context) bool {
	job := service.AsyncJobFromExecutionContext(c.Request.Context())
	if job == nil || c.Request.Method != http.MethodGet || job.NativeTaskID == "" {
		return false
	}
	path := c.Request.URL.Path
	nativeBase := "/v1/tasks/" + job.NativeTaskID
	allowedPath := path == nativeBase || path == nativeBase+"/artifacts" ||
		(strings.HasPrefix(path, nativeBase+"/artifacts/") && strings.HasSuffix(path, "/content")) ||
		path == "/v1/videos/"+job.NativeTaskID+"/content"
	if !allowedPath {
		return false
	}
	var token model.Token
	if job.JobID == "" || job.UserID <= 0 || job.TokenID <= 0 || model.DB.Unscoped().Where("id = ? AND user_id = ?", job.TokenID, job.UserID).First(&token).Error != nil {
		abortWithOpenAiMessage(c, http.StatusForbidden, "Invalid async archival identity")
		return true
	}
	c.Set("id", job.UserID)
	c.Set("token_id", job.TokenID)
	c.Set("token_key", token.Key)
	c.Set("token_name", token.Name)
	c.Set(model.BusinessModalitiesContextKey, []string{})
	metadata := job.BusinessMetadata
	metadata.AsyncTaskID = job.JobID
	model.SetBusinessMetadata(c, metadata)
	model.SetInternalAsyncTaskAccess(c, job.JobID)
	c.Next()
	return true
}

// CheckBusinessModality is shared by synchronous relay and durable job admission.
// Unknown capabilities are rejected for restricted keys; empty policy preserves
// the old unrestricted behavior. Input images are not output-image permission.
func CheckBusinessModality(c *gin.Context, required ...string) error {
	allowed := c.GetStringSlice(model.BusinessModalitiesContextKey)
	if len(allowed) == 0 {
		return nil
	}
	if len(required) == 0 {
		return errors.New("request modality is not classified for this restricted key")
	}
	for _, modality := range required {
		if !slices.Contains(allowed, modality) {
			return fmt.Errorf("API key does not allow %s modality", modality)
		}
	}
	return nil
}

func EnforceBusinessModality(c *gin.Context, required ...string) bool {
	if err := CheckBusinessModality(c, required...); err != nil {
		abortWithOpenAiMessage(c, http.StatusForbidden, err.Error())
		return false
	}
	return true
}

func EnforceBusinessRelayModality(c *gin.Context, modelName string) bool {
	if err := CheckBusinessRelayModality(c, modelName); err != nil {
		abortWithOpenAiMessage(c, http.StatusForbidden, err.Error())
		return false
	}
	return true
}

// Relay attempts repeat this check after channel mapping, so an alias or a
// different retry channel cannot erase the actual model's output capability.
func CheckBusinessRelayModality(c *gin.Context, modelName string) error {
	if len(c.GetStringSlice(model.BusinessModalitiesContextKey)) == 0 {
		return nil
	}
	if c.Request == nil {
		return CheckBusinessModality(c)
	}
	path := c.Request.URL.Path
	if path == "/v1/realtime" {
		return CheckBusinessModality(c, "text", "audio")
	}
	if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
		return nil
	}
	required := []string{}
	switch {
	case strings.Contains(path, "/images/") || path == "/v1/edits" || strings.HasPrefix(path, "/mj/"):
		required = append(required, "image")
	case strings.Contains(path, "/videos") || strings.Contains(path, "/video/"):
		required = append(required, "video")
	case strings.Contains(path, "/audio/"):
		required = append(required, "audio")
	case strings.Contains(path, "/chat/completions") || strings.Contains(path, "/responses") || path == "/v1/completions" || path == "/v1/messages" || strings.Contains(path, "/embeddings") || path == "/v1/rerank" || path == "/v1/moderations" || path == "/v1/alpha/search" || strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent") || strings.Contains(path, ":embedContent") || strings.Contains(path, ":batchEmbedContents"):
		required = append(required, "text")
		modelModalities := BusinessModelModalities(modelName)
		if len(modelModalities) > 0 {
			required = modelModalities
		}
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return CheckBusinessModality(c)
		}
		raw, err := storage.Bytes()
		if err != nil || !gjson.ValidBytes(raw) {
			return CheckBusinessModality(c)
		}
		body := gjson.ParseBytes(raw)
		gemini := strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent")
		if gemini {
			for _, selector := range []string{"generationConfig.responseModalities", "generationConfig.response_modalities", "generation_config.responseModalities", "generation_config.response_modalities"} {
				if body.Get(selector).IsArray() && len(body.Get(selector).Array()) > 0 {
					required = []string{}
					break
				}
			}
		}
		for _, selection := range []gjson.Result{body.Get("modalities"), body.Get("generationConfig.responseModalities"), body.Get("generationConfig.response_modalities"), body.Get("generation_config.responseModalities"), body.Get("generation_config.response_modalities")} {
			if selection.Exists() && !selection.IsArray() && selection.Type != gjson.Null {
				return CheckBusinessModality(c)
			}
			for _, item := range selection.Array() {
				modality := strings.ToLower(item.String())
				if !slices.Contains([]string{"text", "image", "video", "audio"}, modality) {
					return CheckBusinessModality(c)
				}
				if !slices.Contains(required, modality) {
					required = append(required, modality)
				}
			}
		}
		for _, tool := range body.Get("tools").Array() {
			switch tool.Get("type").String() {
			case "image_generation":
				required = append(required, "image")
			case "video_generation":
				required = append(required, "video")
			case "audio_generation":
				required = append(required, "audio")
			}
		}
		required = append(required, modelModalities...)
		// An image-only model exposed through Chat/Gemini must not bypass limits
		// just because an upstream omits the explicit output modality field.
		for _, endpoint := range model.GetModelSupportEndpointTypes(modelName) {
			if endpoint == "image-generation" {
				required = append(required, "image")
			}
			if endpoint == "openai-video" {
				required = append(required, "video")
			}
		}
	}
	// Merge host-owned plugin capabilities even for Chat/Responses adapters:
	// a plugin decoder may rewrite the body, but cannot erase its output gate.
	if pinned, exists := c.Get(jsplugin.ContextKeyPinnedPlugin); exists {
		if plugin, ok := pinned.(jsplugin.PinnedPlugin); ok && plugin.Plugin != nil {
			schema, _ := plugin.Plugin.Meta.UsageForModel(modelName)
			if _, image := schema["image_count"]; image {
				required = append(required, "image")
			}
			action := strings.ToLower(constant.NormalizeTaskAction(c.GetString("task_action")))
			if _, clips := schema["clips"]; clips {
				if strings.Contains(action, "lyrics") {
					required = append(required, "text")
				} else {
					required = append(required, "audio")
				}
			}
			if strings.Contains(action, "video") || action == "remix" {
				required = append(required, "video")
			}
		}
	}
	return CheckBusinessModality(c, required...)
}

// BusinessModelModalities mirrors host-supported image/audio/video model
// families, including Gemini models whose default output can be non-text.
// It is additive to endpoint/tool checks, never a permission supplied by clients.
func BusinessModelModalities(name string) []string {
	name = strings.ToLower(name)
	if common.IsImageGenerationModel(name) {
		return []string{"image"}
	}
	for _, prefix := range []string{"gemini-2.5-flash-image", "gemini-3-pro-image", "gemini-3.1-flash-image", "gemini-3.1-flash-lite-image", "nano-banana", "imagen-"} {
		if strings.HasPrefix(name, prefix) {
			return []string{"image"}
		}
	}
	for _, prefix := range []string{"gemini-2.5-flash-preview-tts", "gemini-2.5-pro-preview-tts", "gemini-2.5-flash-native-audio"} {
		if strings.HasPrefix(name, prefix) {
			return []string{"audio"}
		}
	}
	if strings.HasPrefix(name, "veo-") {
		return []string{"video"}
	}
	return nil
}
