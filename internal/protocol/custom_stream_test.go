package protocol

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// responsesCustomToolStream is one complete Responses stream carrying a
// freeform shell call, with the input split across delta events.
const responsesCustomToolStream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"c1","name":"shell","input":"","status":"in_progress"}}

event: response.custom_tool_call_input.delta
data: {"type":"response.custom_tool_call_input.delta","item_id":"ctc_1","output_index":0,"delta":"ls"}

event: response.custom_tool_call_input.done
data: {"type":"response.custom_tool_call_input.done","item_id":"ctc_1","output_index":0,"input":"ls"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"c1","name":"shell","input":"ls","status":"completed"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"custom_tool_call","call_id":"c1","name":"shell","input":"ls"}]}}

`

func TestCollapseResponsesCustomToolStream(t *testing.T) {
	body, err := CollapseStream(strings.NewReader(responsesCustomToolStream), Responses, "m")
	if err != nil {
		t.Fatalf("CollapseStream: %v", err)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("collapsed response is not JSON: %v", err)
	}
	output, _ := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %#v, want one custom call", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "shell" || item["input"] != "ls" {
		t.Fatalf("custom call = %#v, want the custom shape preserved", item)
	}
	if _, ok := item["arguments"]; ok {
		t.Fatalf("custom call = %#v, want no function arguments", item)
	}
}

func TestTranscodeResponsesCustomToolToChat(t *testing.T) {
	rec := httptest.NewRecorder()
	if _, _, err := TranscodeStream(t.Context(), rec, strings.NewReader(responsesCustomToolStream), Responses, Chat, "m"); err != nil {
		t.Fatalf("TranscodeStream: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"shell"`) {
		t.Fatalf("Chat stream lost the custom tool name:\n%s", body)
	}
	if !strings.Contains(body, `"arguments":"{\"input\":\"ls\"}"`) {
		t.Fatalf("Chat stream did not bridge the custom input as function arguments:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("Chat stream did not report the tool stop:\n%s", body)
	}
}

func TestTranscodeChatCustomToolToResponses(t *testing.T) {
	stream := `data: {"id":"chatcmpl_1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"input\":\"ls\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl_1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	rec := httptest.NewRecorder()
	options := ResponseOptions{CustomTools: map[string]bool{"shell": true}}
	if _, _, err := TranscodeStreamWithOptions(t.Context(), rec, strings.NewReader(stream), Chat, Responses, "m", options); err != nil {
		t.Fatalf("TranscodeStreamWithOptions: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"custom_tool_call"`) {
		t.Fatalf("Responses stream lost the custom tool type:\n%s", body)
	}
	if !strings.Contains(body, `"input":"ls"`) {
		t.Fatalf("Responses stream did not unwrap the custom input:\n%s", body)
	}
	if strings.Contains(body, `"type":"function_call"`) || strings.Contains(body, "function_call_arguments") {
		t.Fatalf("Responses stream emitted a function call for a custom tool:\n%s", body)
	}
	if !strings.Contains(body, `"type":"response.custom_tool_call_input.done"`) {
		t.Fatalf("Responses stream did not close the custom input:\n%s", body)
	}
}

func TestConvertChatCustomToolToResponses(t *testing.T) {
	body := []byte(`{"id":"chatcmpl_1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"input\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`)
	converted, err := ConvertResponseWithOptions(Chat, Responses, body, ResponseOptions{CustomTools: map[string]bool{"shell": true}})
	if err != nil {
		t.Fatalf("ConvertResponseWithOptions: %v", err)
	}
	var response map[string]any
	if err := json.Unmarshal(converted, &response); err != nil {
		t.Fatalf("converted response is not JSON: %v", err)
	}
	output, _ := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %#v, want one custom call", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "shell" || item["input"] != "ls" {
		t.Fatalf("custom call = %#v, want the custom shape", item)
	}
	if _, ok := item["arguments"]; ok {
		t.Fatalf("custom call = %#v, want no function arguments", item)
	}
}

func TestCustomToolArgumentsRoundTrip(t *testing.T) {
	for _, input := range []string{"ls", `{"a":1}`, `{"input":"x"}`, "", "line one\nline two"} {
		arguments := customToolArguments(input)
		if got := customToolInput(arguments); got != input {
			t.Fatalf("customToolInput(customToolArguments(%q)) = %q; want the input back", input, got)
		}
	}
}
