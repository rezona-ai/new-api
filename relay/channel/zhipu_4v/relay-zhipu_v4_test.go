package zhipu_4v

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
)

func TestRequestOpenAI2ZhipuPreservesZhipuParams(t *testing.T) {
	t.Parallel()

	thinking, err := common.Marshal(map[string]string{"type": "enabled"})
	if err != nil {
		t.Fatalf("marshal thinking: %v", err)
	}
	reasoning := "previous chain of thought"

	in := dto.GeneralOpenAIRequest{
		Model: "glm-5.3-flash",
		Messages: []dto.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello", ReasoningContent: &reasoning},
		},
		THINKING:        thinking,
		ReasoningEffort: "low",
		ResponseFormat:  &dto.ResponseFormat{Type: "json_object"},
	}

	out := requestOpenAI2Zhipu(in)
	if out == nil {
		t.Fatal("requestOpenAI2Zhipu returned nil")
	}
	if out.ReasoningEffort != "low" {
		t.Fatalf("ReasoningEffort = %q, want %q", out.ReasoningEffort, "low")
	}
	if len(out.THINKING) == 0 {
		t.Fatal("THINKING was dropped")
	}
	if out.ResponseFormat == nil || out.ResponseFormat.Type != "json_object" {
		t.Fatalf("ResponseFormat = %#v, want json_object", out.ResponseFormat)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(out.Messages))
	}
	gotReasoning := out.Messages[1].ReasoningContent
	if gotReasoning == nil || *gotReasoning != reasoning {
		t.Fatalf("Messages[1].ReasoningContent = %#v, want %q", gotReasoning, reasoning)
	}

	body, err := common.Marshal(out)
	if err != nil {
		t.Fatalf("marshal converted request: %v", err)
	}
	raw := string(body)
	for _, want := range []string{`"reasoning_effort":"low"`, `"response_format"`, `"reasoning_content"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("marshaled body missing %s: %s", want, raw)
		}
	}
}
