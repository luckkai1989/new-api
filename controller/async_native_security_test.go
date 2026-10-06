package controller

import (
	"context"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAsyncNativeArtifactCannotDisablePublicNetworkBoundary(t *testing.T) {
	task := setupGenericTaskTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Token{}))
	require.NoError(t, model.DB.Create(&model.Token{Id: 7331, UserId: task.UserId, Key: "synthetic-async-artifact-key", Status: common.TokenStatusEnabled}).Error)
	previous := *system_setting.GetFetchSetting()
	system_setting.GetFetchSetting().EnableSSRFProtection = false
	system_setting.GetFetchSetting().AllowPrivateIp = true
	t.Cleanup(func() { *system_setting.GetFetchSetting() = previous; service.SetAsyncRelayExecutor(nil) })
	job := &model.AsyncJob{JobID: "async-artifact-security", UserID: task.UserId, TokenID: 7331, NativeTaskID: task.TaskID}
	task.AsyncJobID = job.JobID
	var proxyErr error
	service.SetAsyncRelayExecutor(func(writer http.ResponseWriter, request *http.Request) {
		c, _ := gin.CreateTestContext(writer)
		c.Request = request
		proxyErr = proxyTaskMedia(c, task, &relaychannel.TaskContentRequest{URL: "http://127.0.0.1/private-result", Method: http.MethodGet})
		writer.WriteHeader(http.StatusBadGateway)
	})
	_, err := service.ExecuteAsyncRelay(context.Background(), job, "/v1/tasks/"+task.TaskID+"/artifacts/output/content", http.MethodGet, nil)
	require.NoError(t, err)
	var rejection *taskMediaProxyError
	require.ErrorAs(t, proxyErr, &rejection)
	require.Equal(t, "artifact_request_rejected", rejection.code)
	// The worker-only boundary also applies with a configured legacy proxy;
	// it never falls back to the legacy proxy's relaxed address policy.
	channel, err := model.GetChannelById(task.ChannelId, true)
	require.NoError(t, err)
	proxySetting := `{"proxy":"http://127.0.0.1:3128"}`
	channel.Setting = &proxySetting
	require.NoError(t, model.DB.Model(channel).Update("setting", channel.Setting).Error)
	_, err = service.ExecuteAsyncRelay(context.Background(), job, "/v1/tasks/"+task.TaskID+"/artifacts/output/content", http.MethodGet, nil)
	require.NoError(t, err)
	require.ErrorAs(t, proxyErr, &rejection)
	require.Equal(t, "artifact_request_rejected", rejection.code)
}
