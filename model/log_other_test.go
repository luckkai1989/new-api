package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBusinessAsyncLogsNeverExposeResultsForAnyRole(t *testing.T) {
	for _, visibility := range []string{"user", "admin", "root"} {
		t.Run(visibility, func(t *testing.T) {
			log := &Log{BusinessMetadata: BusinessMetadata{AsyncTaskID: "async-log", TagLevel1: "system"}, Content: "failed to store https://provider.invalid/file?signature=secret", Other: `{"model_ratio":2,"artifact_url":"https://provider.invalid/file","data":[{"b64_json":"private-output-canary"}],"root_info":{"request_path":"/v1/images/generations","nested":{"url":"https://provider.invalid/file"}}}`}
			switch visibility {
			case "user":
				formatUserLogs([]*Log{log}, 0)
			case "admin":
				FormatAdminLogs([]*Log{log})
			case "root":
				FormatRootLogs([]*Log{log})
			}
			assert.NotContains(t, log.Content+log.Other, "provider.invalid")
			assert.NotContains(t, log.Other, "private-output-canary")
			assert.Contains(t, log.Other, `"model_ratio":2`)
			assert.Equal(t, "system", log.TagLevel1)
		})
	}
}

func TestLogOtherScopesAndMerges(t *testing.T) {
	var other LogOther

	assert.True(t, other.SetPublic("request_path", "/v1/chat/completions"))
	other.MergePublic(map[string]any{
		"zero": 0,
	})
	assert.True(t, other.SetAdmin("use_channel", []string{"channel-a"}))
	other.MergeAdmin(map[string]any{
		"rejected": false,
	})
	assert.True(t, other.SetRoot("upstream_request_id", "upstream-private"))
	other.MergeRoot(map[string]any{
		"generation": 0,
	})
	assert.True(t, other.SetAudit("method", "POST"))
	other.MergeAudit(map[string]any{
		"success": false,
	})

	require.JSONEq(t, `{
		"request_path": "/v1/chat/completions",
		"zero": 0,
		"admin_info": {
			"use_channel": ["channel-a"],
			"rejected": false
		},
		"root_info": {
			"upstream_request_id": "upstream-private",
			"generation": 0
		},
		"audit_info": {
			"method": "POST",
			"success": false
		}
	}`, other.JSONString())
}

func TestLogOtherRejectsSensitivePublicFields(t *testing.T) {
	other := NewLogOther()

	for _, key := range []string{
		"admin_info",
		"root_info",
		"audit_info",
		"channel_id",
		"channel_name",
		"channel_type",
		"reject_reason",
	} {
		assert.False(t, other.SetPublic(key, "must-not-leak"), key)
	}
	other.MergePublic(map[string]any{
		"request_path": "/v1/responses",
		"channel_name": "still-must-not-leak",
		"admin_info":   map[string]any{"secret": true},
	})

	require.JSONEq(t, `{"request_path":"/v1/responses"}`, other.JSONString())
	require.JSONEq(t, `{}`, NewLogOther().JSONString())
}

func TestLogOtherJSONStringDoesNotMutateReceiver(t *testing.T) {
	other := NewLogOther()
	require.True(t, other.SetPublic("request_path", "/v1/chat/completions"))
	require.True(t, other.SetAdmin("rejected", false))

	before := other.Snapshot()
	first := other.JSONString()
	after := other.Snapshot()
	second := other.JSONString()

	require.Equal(t, before, after)
	require.Equal(t, first, second)
}
