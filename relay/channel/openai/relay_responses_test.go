package openai

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOaiResponsesStreamHandler_UpstreamInputZeroOutputKeepsCompletionZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() {
		constant.StreamingTimeout = oldStreamingTimeout
	})

	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-4o",
		},
	}
	info.SetEstimatePromptTokens(999)

	delta, err := common.Marshal(dto.ResponsesStreamResponse{
		Type:  "response.output_text.delta",
		Delta: "hello world this is a long enough reply to produce local tokens",
	})
	require.NoError(t, err)

	completed, err := common.Marshal(dto.ResponsesStreamResponse{
		Type: "response.completed",
		Response: &dto.OpenAIResponsesResponse{
			Usage: &dto.Usage{
				InputTokens:  100,
				OutputTokens: 0,
				TotalTokens:  100,
			},
		},
	})
	require.NoError(t, err)

	streamBody := []byte("data: " + string(delta) + "\n" + "data: " + string(completed) + "\n" + "data: [DONE]\n")
	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(streamBody)),
	}

	usage, newAPIError := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 100, usage.PromptTokens)
	require.Equal(t, 0, usage.CompletionTokens)
	require.Equal(t, 100, usage.TotalTokens)
	require.False(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
}

func TestOaiResponsesStreamHandler_NoUpstreamUsageUsesLocalEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() {
		constant.StreamingTimeout = oldStreamingTimeout
	})

	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-4o",
		},
	}
	info.SetEstimatePromptTokens(999)

	delta, err := common.Marshal(dto.ResponsesStreamResponse{
		Type:  "response.output_text.delta",
		Delta: "hello world this is a long enough reply to produce local tokens",
	})
	require.NoError(t, err)

	streamBody := []byte("data: " + string(delta) + "\n" + "data: [DONE]\n")
	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(streamBody)),
	}

	usage, newAPIError := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 999, usage.PromptTokens)
	require.Greater(t, usage.CompletionTokens, 0)
	require.True(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
}
