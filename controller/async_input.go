package controller

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

var errAsyncExternalMedia = errors.New("Async media inputs must be multipart file bytes or supported inline data; external URLs and provider file IDs are not durable inputs")

// Do not enqueue transient provider URLs or file handles: a delayed execution
// cannot guarantee their lifetime. Text prompts may still discuss normal URLs.
// Unknown extension containers are inspected too, rather than trusting just
// the top-level fields understood by today's adapters.
func validateAsyncDurableInputs(endpoint, contentType string, body []byte) error {
	textInput := endpoint == "/v1/audio/speech"
	mediaType, params, _ := mime.ParseMediaType(contentType)
	if mediaType == "multipart/form-data" {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if part.FileName() != "" {
				if _, err := io.Copy(io.Discard, part); err != nil {
					return err
				}
				continue
			}
			raw, err := io.ReadAll(part)
			if err != nil {
				return err
			}
			if err := validateAsyncInputValue(part.FormName(), string(raw), true, textInput, 0); err != nil {
				return err
			}
		}
	}
	var request map[string]any
	if err := common.Unmarshal(body, &request); err != nil || request == nil {
		return errors.New("request must be a JSON object")
	}
	for key, value := range request {
		if err := validateAsyncInputValue(key, value, true, textInput, 0); err != nil {
			return err
		}
	}
	return nil
}

func validateAsyncInputValue(key string, value any, topLevel, textInput bool, depth int) error {
	if depth > 64 {
		return errAsyncExternalMedia
	}
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.TrimSuffix(key, "[]")
	// A provider-owned identifier is not a copy of the input. Even a non-URL
	// value under these fields must not be deferred for later resolution.
	if key == "file_id" || key == "file_ids" || key == "image_id" || key == "audio_id" || key == "video_id" {
		if value != nil && value != "" {
			return errAsyncExternalMedia
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		for childKey, child := range typed {
			if err := validateAsyncInputValue(childKey, child, false, textInput, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validateAsyncInputValue(key, child, false, textInput, depth+1); err != nil {
				return err
			}
		}
	case string:
		if key == "prompt" || key == "negative_prompt" || key == "instructions" || key == "text" || key == "ref_text" || topLevel && textInput && key == "input" {
			return nil
		}
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		// Multipart extension values commonly contain another JSON object.
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var nested any
			if common.Unmarshal([]byte(trimmed), &nested) == nil {
				return validateAsyncInputValue(key, nested, false, textInput, depth+1)
			}
		}
		lower := strings.ToLower(trimmed)
		for _, scheme := range []string{"http://", "https://", "gs://", "s3://", "oss://", "file://"} {
			if strings.Contains(lower, scheme) {
				return errAsyncExternalMedia
			}
		}
		if strings.HasPrefix(lower, "//") || strings.HasPrefix(lower, "file-") || strings.HasPrefix(lower, "file_") {
			return errAsyncExternalMedia
		}
		switch key {
		case "image", "images", "mask", "audio", "ref_audio", "video", "input_reference", "image_url", "audio_url", "video_url", "img_url", "url", "image_tail", "first_frame_url", "last_frame_url", "file":
			if !asyncInlineMedia(trimmed) {
				return errAsyncExternalMedia
			}
		}
	}
	return nil
}

func asyncInlineMedia(value string) bool {
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		header, body, found := strings.Cut(value, ",")
		header = strings.ToLower(header)
		if !found || !strings.HasSuffix(header, ";base64") || !(strings.HasPrefix(header, "data:image/") || strings.HasPrefix(header, "data:audio/") || strings.HasPrefix(header, "data:video/") || header == "data:application/octet-stream;base64") {
			return false
		}
		value = body
	}
	if value == "" {
		return false
	}
	_, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)))
	return err == nil
}
