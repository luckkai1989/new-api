package controller

import (
	"bytes"
	"mime/multipart"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsyncAdmissionRejectsExternalMediaBeforeAnyDurableWrite(t *testing.T) {
	store := asyncAdmissionFixture(t)
	for _, media := range []string{
		`"image":"https://example.org/temporary.png"`,
		`"images":["//example.org/image.png"]`,
		`"ref_audio":{"file_id":"provider-123"}`,
		`"metadata":{"image_tail":"oss://bucket/image"}`,
		`"input":[{"type":"image_url","image_url":{"url":"http://example.org/a"}}]`,
		`"extensions":{"unknown":{"media":[{"url":"s3://bucket/key"}]}}`,
		`"extra_fields":{"input":{"file_ids":["abc"]}}`,
		`"image":"provider-image-handle"`,
		`"extensions":{"source":"file-123"}`,
		`"input":"https://example.org/temporary-input"`,
	} {
		body := []byte(`{"endpoint":"/v1/images/generations","request":{"model":"image-model",` + media + `}}`)
		c, recorder := submitAsyncTestContext("application/json", body)
		SubmitAsyncTask(c)
		assert.Equal(t, 400, recorder.Code, string(body)+recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "external_async_input_not_supported")
	}
	var jobs int64
	require.NoError(t, model.DB.Model(&model.AsyncJob{}).Count(&jobs).Error)
	assert.Zero(t, jobs)
	assert.Empty(t, store.bodies)
}

func TestAsyncAdmissionMultipartInspectsNonFileExtensionFields(t *testing.T) {
	store := asyncAdmissionFixture(t)
	for _, field := range [][2]string{{"image", "https://example.org/temporary.png"}, {"extensions", `{"media":[{"url":"gs://bucket/object"}]}`}, {"input_reference", "file-123"}} {
		var raw bytes.Buffer
		writer := multipart.NewWriter(&raw)
		for _, item := range [][2]string{{"endpoint", "/v1/images/edits"}, {"model", "image-model"}, {"prompt", "show https://example.org in the design"}, field} {
			require.NoError(t, writer.WriteField(item[0], item[1]))
		}
		file, err := writer.CreateFormFile("image[]", "image.png")
		require.NoError(t, err)
		_, err = file.Write([]byte("durable-file-bytes"))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		c, recorder := submitAsyncTestContext(writer.FormDataContentType(), raw.Bytes())
		SubmitAsyncTask(c)
		assert.Equal(t, 400, recorder.Code, recorder.Body.String())
		assert.Contains(t, recorder.Body.String(), "external_async_input_not_supported")
	}
	assert.Empty(t, store.bodies)
}

func TestAsyncDurableInputsPermitTextURLsAndInlineBytes(t *testing.T) {
	for _, request := range []map[string]any{
		{"prompt": "a poster about https://example.org", "image": "data:image/png;base64,aW1hZ2U="},
		{"input": "read https://example.org out loud", "instructions": "say https://example.org"},
		{"images": []any{"aW1hZ2U="}, "extensions": map[string]any{"quality": "high"}},
	} {
		body, err := common.Marshal(request)
		require.NoError(t, err)
		require.NoError(t, validateAsyncDurableInputs("/v1/audio/speech", "application/json", body))
	}
	for _, request := range []string{`{"image":"data:image/png;base64,invalid_!"}`, `{"image":"data:text/html;base64,aW1hZ2U="}`} {
		require.Error(t, validateAsyncDurableInputs("/v1/images/generations", "application/json", []byte(request)))
	}
}

func TestAsyncDurableInputsOnlySpeechTreatsInputAsText(t *testing.T) {
	for _, endpoint := range []string{"/v1/videos", "/v1/images/generations", "/v1/images/edits"} {
		require.Error(t, validateAsyncDurableInputs(endpoint, "application/json", []byte(`{"input":"https://example.org/transient-media"}`)))
	}
	require.NoError(t, validateAsyncDurableInputs("/v1/audio/speech", "application/json", []byte(`{"input":"Read https://example.org out loud","ref_text":"https://example.org"}`)))
}
