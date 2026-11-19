package protocol

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode2api/internal/jsonutil"
)

func mustConvert(t *testing.T, from, to Protocol, input map[string]any) map[string]any {
	t.Helper()
	out, err := ConvertRequest(from, to, input)
	if err != nil {
		t.Fatalf("convert %s->%s: %v", from, to, err)
	}
	return out
}

func TestReasoningLevels(t *testing.T) {
	base := func(effort any) map[string]any {
		return map[string]any{
			"model": "test", "stream": false,
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": effort,
		}
	}

	t.Run("chat max to anthropic", func(t *testing.T) {
		out := mustConvert(t, Chat, Anthropic, base("max"))
		if got := jsonutil.StringAt(out, "output_config", "effort"); got != "max" {
			t.Fatalf("effort = %q, want max", got)
		}
		thinking, _ := out["thinking"].(map[string]any)
		if thinking == nil || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(32768) {
			t.Fatalf("thinking = %#v", thinking)
		}
	})

	t.Run("anthropic max keeps budget and round trips", func(t *testing.T) {
		input := map[string]any{
			"model": "test", "stream": false, "max_tokens": 64000,
			"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
			"thinking":      map[string]any{"type": "enabled", "budget_tokens": float64(32000)},
			"output_config": map[string]any{"effort": "max"},
		}
		out := mustConvert(t, Anthropic, Chat, input)
		if got := out["reasoning_effort"]; got != "max" {
			t.Fatalf("reasoning_effort = %#v, want max", got)
		}
		encoded, _ := json.Marshal(out)
		if !strings.Contains(string(encoded), `"reasoning_budget_tokens":32000`) {
			t.Fatalf("budget not carried on chat body: %s", encoded)
		}
		back := mustConvert(t, Chat, Anthropic, out)
		thinking, _ := back["thinking"].(map[string]any)
		if thinking == nil || thinking["budget_tokens"] != float64(32000) {
			t.Fatalf("round-trip thinking = %#v", thinking)
		}
		if got := jsonutil.StringAt(back, "output_config", "effort"); got != "max" {
			t.Fatalf("round-trip effort = %q", got)
		}
	})

	t.Run("disabled stays disabled", func(t *testing.T) {
		out := mustConvert(t, Chat, Anthropic, base("none"))
		thinking, _ := out["thinking"].(map[string]any)
		if thinking == nil || thinking["type"] != "disabled" {
			t.Fatalf("thinking = %#v, want disabled", thinking)
		}
	})

	t.Run("anthropic budget maps to level", func(t *testing.T) {
		input := map[string]any{
			"model": "test", "stream": false,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(32000)},
		}
		out := mustConvert(t, Anthropic, Chat, input)
		if got := out["reasoning_effort"]; got != "xhigh" {
			t.Fatalf("reasoning_effort = %#v, want xhigh", got)
		}
	})
}

func TestForcedEffort(t *testing.T) {
	t.Run("chat fills unset", func(t *testing.T) {
		body := map[string]any{"model": "test"}
		ForcedEffort(Chat, body, "high")
		if body["reasoning_effort"] != "high" {
			t.Fatalf("body = %#v", body)
		}
	})

	t.Run("chat keeps client level", func(t *testing.T) {
		body := map[string]any{"model": "test", "reasoning_effort": "low"}
		ForcedEffort(Chat, body, "high")
		if body["reasoning_effort"] != "low" {
			t.Fatalf("body = %#v", body)
		}
	})

	t.Run("chat disabled is not overridden", func(t *testing.T) {
		body := map[string]any{"model": "test", "reasoning_effort": "none"}
		ForcedEffort(Chat, body, "high")
		if body["reasoning_effort"] != "none" {
			t.Fatalf("body = %#v", body)
		}
	})

	t.Run("anthropic keeps max_tokens above budget", func(t *testing.T) {
		body := map[string]any{"model": "test", "max_tokens": float64(1000)}
		ForcedEffort(Anthropic, body, "max")
		thinking, _ := body["thinking"].(map[string]any)
		if thinking == nil || thinking["budget_tokens"] != float64(32768) {
			t.Fatalf("thinking = %#v", thinking)
		}
		if got := jsonutil.IntAt(body, "max_tokens"); got != 32768+4096 {
			t.Fatalf("max_tokens = %d", got)
		}
		if got := jsonutil.StringAt(body, "output_config", "effort"); got != "max" {
			t.Fatalf("effort = %q", got)
		}
	})

	t.Run("anthropic keeps client effort", func(t *testing.T) {
		body := map[string]any{"model": "test", "output_config": map[string]any{"effort": "minimal"}}
		ForcedEffort(Anthropic, body, "max")
		if got := jsonutil.StringAt(body, "output_config", "effort"); got != "minimal" {
			t.Fatalf("effort = %q", got)
		}
		if _, ok := body["thinking"]; ok {
			t.Fatalf("thinking injected despite client effort: %#v", body)
		}
	})
}

func TestFinishReasonFastPath(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`, false},
		{`{"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}`, true},
		{`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, true},
		{`{"choices":[{"delta":{"content":"x \"finish_reason\": null y"}}]}`, false},
		{`{"choices":[{"finish_reason" :  "length" }]}`, true},
	}
	for _, tc := range cases {
		if got := hasChatFinishReason(tc.raw); got != tc.want {
			t.Errorf("hasChatFinishReason(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestReadSSEFraming(t *testing.T) {
	raw := "event: message\r\ndata: {\"a\":1}\r\n\r\n" +
		": a comment\n\n" +
		"data: line1\ndata: line2\n\n" +
		"data: [DONE]\n\n"
	var got []string
	err := readSSE(strings.NewReader(raw), func(event, data string) error {
		got = append(got, event+"|"+data)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	want := []string{`message|{"a":1}`, "|line1\nline2", "|[DONE]"}
	if len(got) != len(want) {
		t.Fatalf("events = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestReadSSETruncatedRecord(t *testing.T) {
	err := readSSE(strings.NewReader("data: {\"a\":1}\n"), func(string, string) error { return nil })
	if err != errSSEUnexpectedEOF {
		t.Fatalf("err = %v, want errSSEUnexpectedEOF", err)
	}
}

func TestRenameStreamReassemblesFragmentedName(t *testing.T) {
	renames := NewToolRenames()
	renames.Add("my_tool", "mcp_very_long_wire_name")
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"mcp_very\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"_long_wire_name\",\"arguments\":\"{\\\"x\\\":1}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	var out strings.Builder
	if _, err := io.Copy(&out, RenameStreamReader(strings.NewReader(stream), Chat, renames)); err != nil {
		t.Fatalf("read renamed stream: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, `"my_tool"`) {
		t.Fatalf("renamed stream missing client name:\n%s", text)
	}
	if strings.Contains(text, "mcp_very") || strings.Contains(text, "long_wire_name") {
		t.Fatalf("wire name leaked:\n%s", text)
	}
	if !strings.Contains(text, `\"x\":1`) {
		t.Fatalf("arguments lost:\n%s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with DONE:\n%s", text)
	}
}

func TestForwardStreamPassThrough(t *testing.T) {
	stream := "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if _, _, err := ForwardStream(t.Context(), rec, strings.NewReader(stream), Chat, "test"); err != nil {
		t.Fatalf("ForwardStream: %v", err)
	}
	if rec.Body.String() != stream {
		t.Fatalf("body changed:\n%s", rec.Body.String())
	}
}

func TestForwardStreamMalformedBeforeCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	_, _, err := ForwardStream(t.Context(), rec, strings.NewReader("data: {bad}\n\n"), Chat, "test")
	if err == nil {
		t.Fatal("expected an error")
	}
	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestTranscodeChatToAnthropic(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl_1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	_, _, err := TranscodeStream(t.Context(), rec, strings.NewReader(stream), Chat, Anthropic, "m")
	if err != nil {
		t.Fatalf("TranscodeStream: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		`"text":"hi"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("anthropic stream must not carry [DONE]:\n%s", body)
	}
}

func TestTranscodeAnthropicToChatReasoning(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rec := httptest.NewRecorder()
	_, _, err := TranscodeStream(t.Context(), rec, strings.NewReader(stream), Anthropic, Chat, "m")
	if err != nil {
		t.Fatalf("TranscodeStream: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"reasoning_content":"plan"`) {
		t.Fatalf("reasoning missing:\n%s", body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("text missing:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("finish reason missing:\n%s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("chat stream must end with [DONE]:\n%s", body)
	}
}
