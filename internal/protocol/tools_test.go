package protocol

import (
	"encoding/json"
	"testing"

	"opencode2api/internal/jsonutil"
)

func TestParseTextToolCallNestedFunctionArguments(t *testing.T) {
	calls, ok := parseTextToolCalls(`<tool_call>{"function":{"name":"foo","arguments":{"cmd":"ls"}}}</tool_call>`, nil)
	if !ok || len(calls) != 1 {
		t.Fatalf("parseTextToolCalls = %v, %v; want one call", calls, ok)
	}
	if calls[0].Name != "foo" {
		t.Fatalf("name = %q, want foo", calls[0].Name)
	}
	if calls[0].Arguments != `{"cmd":"ls"}` {
		t.Fatalf("arguments = %q, want nested function.arguments", calls[0].Arguments)
	}
}

func TestParseTextTagArgumentsSelfClosingParameter(t *testing.T) {
	var got map[string]any
	if err := json.Unmarshal([]byte(parseTextTagArguments(`<flag/><cmd>ls</cmd>`)), &got); err != nil {
		t.Fatalf("arguments are not a JSON object: %v", err)
	}
	if value, ok := got["flag"]; !ok || value != "" {
		t.Fatalf("flag = %#v, want empty value", got["flag"])
	}
	if got["cmd"] != "ls" {
		t.Fatalf("cmd = %#v, want ls; self-closing tag swallowed later parameters: %#v", got["cmd"], got)
	}
}

func TestDecodeResponsesCustomToolRequest(t *testing.T) {
	input := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "shell", "input": "ls"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": "ok"},
		},
		"tools": []any{map[string]any{"type": "custom", "name": "shell", "description": "run"}},
	}
	request, err := decodeBridgeRequest(Responses, input)
	if err != nil {
		t.Fatalf("decodeBridgeRequest: %v", err)
	}
	if len(request.Tools) != 1 || request.Tools[0].Name != "shell" {
		t.Fatalf("tools = %#v, want the custom tool bridged", request.Tools)
	}
	schemaMap, ok := request.Tools[0].Schema.(map[string]any)
	if !ok {
		t.Fatalf("custom tool schema = %#v, want an object", request.Tools[0].Schema)
	}
	if _, ok := jsonutil.MapAt(schemaMap, "properties")["input"]; !ok {
		t.Fatalf("custom tool schema = %#v, want an input parameter", schemaMap)
	}
	if len(request.Messages) != 2 {
		t.Fatalf("messages = %#v, want a call and its result", request.Messages)
	}
	call := request.Messages[0].Blocks[0]
	if call.Kind != "tool_call" || call.Name != "shell" || call.ArgumentsJSON != `{"input":"ls"}` {
		t.Fatalf("custom call = %#v", call)
	}
	result := request.Messages[1].Blocks[0]
	if result.Kind != "tool_result" || result.CallID != "c1" || result.Result != "ok" {
		t.Fatalf("custom result = %#v", result)
	}

	converted, err := ConvertRequest(Responses, Chat, input)
	if err != nil {
		t.Fatalf("ConvertRequest Responses->Chat: %v", err)
	}
	tools := jsonutil.SliceAt(converted, "tools")
	if len(tools) != 1 {
		t.Fatalf("converted tools = %#v", tools)
	}
	function, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if jsonutil.StringAt(function, "name") != "shell" {
		t.Fatalf("converted tool = %#v", tools[0])
	}
}

func TestDecodeResponsesCustomToolResponse(t *testing.T) {
	body := map[string]any{
		"id":     "resp_1",
		"status": "completed",
		"output": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "shell", "input": "ls"},
		},
	}
	response, err := decodeBridgeResponse(Responses, body)
	if err != nil {
		t.Fatalf("decodeBridgeResponse: %v", err)
	}
	if len(response.Tools) != 1 {
		t.Fatalf("tools = %#v, want one custom call", response.Tools)
	}
	call := response.Tools[0]
	if call.Name != "shell" || call.ID != "c1" || call.ArgumentsJSON != `{"input":"ls"}` {
		t.Fatalf("custom call = %#v", call)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertResponse(Responses, Chat, raw); err != nil {
		t.Fatalf("ConvertResponse Responses->Chat: %v", err)
	}
}
