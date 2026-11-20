package protocol

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode2api/internal/jsonutil"
)

// extractOne runs extractTextToolCalls and asserts it yielded exactly one call.
func extractOne(t *testing.T, text string) (string, string, string) {
	t.Helper()
	clean, calls := extractTextToolCalls(text, nil)
	if len(calls) != 1 {
		t.Fatalf("calls = %#v (text %q, clean %q)", calls, text, clean)
	}
	return calls[0].Name, calls[0].Arguments, clean
}

// argumentsValue decodes the JSON arguments the parser produced.
func argumentsValue(t *testing.T, arguments string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		t.Fatalf("arguments %q are not a JSON object: %v", arguments, err)
	}
	return decoded
}

// TestExtractSelfClosingCallInsideRoot pins the recovery of a self-closing
// <tool_call/> nested in a <tool_calls> wrapper. Before the fix, the parse
// loop only searched for a closing tag after the opening one; a
// self-closing inner call has no body, the search failed, and the whole
// block reached the client as ordinary text.
func TestExtractSelfClosingCallInsideRoot(t *testing.T) {
	name, arguments, clean := extractOne(t,
		`<tool_calls><tool_call name="shell" arguments='{"cmd":"ls"}'/></tool_calls>`)
	if name != "shell" {
		t.Fatalf("name = %q, want shell", name)
	}
	if got := argumentsValue(t, arguments)["cmd"]; got != "ls" {
		t.Fatalf("cmd = %#v, want ls", got)
	}
	if clean != "" {
		t.Fatalf("clean text = %q, want the block removed", clean)
	}
}

// TestExtractFunctionCallAliasRoot pins the alias root spelling with its own
// closing tag: a raw <function_calls> block is closed by </function_calls>,
// not by the canonical spelling the parser folds it onto.
func TestExtractFunctionCallAliasRoot(t *testing.T) {
	name, arguments, clean := extractOne(t,
		`<function_calls><invoke name="shell"><parameter name="cmd">ls</parameter></invoke></function_calls>`)
	if name != "shell" {
		t.Fatalf("name = %q, want shell", name)
	}
	if got := argumentsValue(t, arguments)["cmd"]; got != "ls" {
		t.Fatalf("cmd = %#v, want ls", got)
	}
	if clean != "" {
		t.Fatalf("clean text = %q, want the block removed", clean)
	}
}

// TestExtractBareInvokeBlock pins a bare Claude <invoke> block without a
// <function_calls> wrapper. Before the fix, no scanner marker matched
// <invoke outside an alias root, so the block was forwarded as text.
func TestExtractBareInvokeBlock(t *testing.T) {
	name, arguments, clean := extractOne(t,
		`<invoke name="bash"><parameter name="cmd">ls</parameter></invoke>`)
	if name != "bash" {
		t.Fatalf("name = %q, want bash", name)
	}
	if got := argumentsValue(t, arguments)["cmd"]; got != "ls" {
		t.Fatalf("cmd = %#v, want ls", got)
	}
	if clean != "" {
		t.Fatalf("clean text = %q, want the block removed", clean)
	}
}

// TestExtractAngleBracketInsideAttribute pins that a ">" inside a quoted
// attribute value cannot end an opening tag early.
func TestExtractAngleBracketInsideAttribute(t *testing.T) {
	name, arguments, _ := extractOne(t,
		`<tool_call name="compare" arguments="a > b"/>`)
	if name != "compare" {
		t.Fatalf("name = %q, want compare", name)
	}
	if got := argumentsValue(t, arguments)["input"]; got != "a > b" {
		t.Fatalf("input = %#v, want the untouched value", got)
	}
}

// TestExtractSelfCloseInsideAttribute pins that a literal "/>" inside a
// quoted attribute value cannot make the scanner treat the tag as
// self-closed at the wrong offset.
func TestExtractSelfCloseInsideAttribute(t *testing.T) {
	name, arguments, _ := extractOne(t,
		`<tool_call name="run" arguments='{"re":"x/>y"}'/>`)
	if name != "run" {
		t.Fatalf("name = %q, want run", name)
	}
	if got := argumentsValue(t, arguments)["re"]; got != "x/>y" {
		t.Fatalf("re = %#v, want the untouched value", got)
	}
}

// TestTagNameNotShadowedByAttributeValue pins that an attribute name is
// matched at its segment start only: a "name=" inside another attribute's
// value must not be read as the name attribute.
func TestTagNameNotShadowedByAttributeValue(t *testing.T) {
	name, arguments, _ := extractOne(t,
		`<tool_call name="shell" arguments="opts name=quiet"/>`)
	if name != "shell" {
		t.Fatalf("name = %q, want shell, not a fragment of the value", name)
	}
	if got := argumentsValue(t, arguments)["input"]; got != "opts name=quiet" {
		t.Fatalf("input = %#v, want the untouched value", got)
	}
}

// TestExtractTagDialectCallKeepsTrailingText keeps the tag dialect end to
// end: the block is removed while the text around it survives untouched.
func TestExtractTagDialectCallKeepsTrailingText(t *testing.T) {
	clean, calls := extractTextToolCalls(
		"before <tool_call name=\"bash\"><parameter name=\"cmd\">ls -la</parameter></tool_call> after", nil)

	if len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("calls = %#v", calls)
	}
	if got := argumentsValue(t, calls[0].Arguments)["cmd"]; got != "ls -la" {
		t.Fatalf("cmd = %#v, want ls -la", got)
	}
	if !strings.HasPrefix(clean, "before") || !strings.HasSuffix(clean, "after") {
		t.Fatalf("clean = %q, want the text around the block kept", clean)
	}
}

// TestExtractChildTagNameDialect pins the tag dialect where the tool name is
// the first child tag instead of a name attribute. The opening tag's ">" must
// not leak into the name or swallow the first character of the arguments.
func TestExtractChildTagNameDialect(t *testing.T) {
	name, arguments, clean := extractOne(t,
		`<tool_call><bash><cmd>ls -la</cmd></bash></tool_call>`)
	if name != "bash" {
		t.Fatalf("name = %q, want bash", name)
	}
	if got := argumentsValue(t, arguments)["cmd"]; got != "ls -la" {
		t.Fatalf("cmd = %#v, want ls -la", got)
	}
	if clean != "" {
		t.Fatalf("clean text = %q, want the block removed", clean)
	}
}

// TestTagAttributePreservesValueCase pins that matching an attribute name
// case-insensitively does not lowercase the value: tool names and JSON
// arguments are case-sensitive.
func TestTagAttributePreservesValueCase(t *testing.T) {
	name, arguments, _ := extractOne(t,
		`<tool_call name="ReadFile" arguments='{"path":"Src/Main.go"}'/>`)
	if name != "ReadFile" {
		t.Fatalf("name = %q, want ReadFile", name)
	}
	if got := argumentsValue(t, arguments)["path"]; got != "Src/Main.go" {
		t.Fatalf("path = %#v, want the original case", got)
	}
}

// TestExtractSelfClosingParameterName pins the common <parameter name="flag"/>
// spelling: the self-closing slash must not become part of the parameter key.
func TestExtractSelfClosingParameterName(t *testing.T) {
	name, arguments, _ := extractOne(t,
		`<tool_call name="shell"><parameter name="verbose"/></tool_call>`)
	if name != "shell" {
		t.Fatalf("name = %q, want shell", name)
	}
	decoded := argumentsValue(t, arguments)
	if _, ok := decoded["verbose"]; !ok {
		t.Fatalf("arguments = %#v, want the verbose key", decoded)
	}
}

// TestRewriteTextToolCallsBareInvoke pins the non-streaming rewrite path: a
// response whose content carries only a bare <invoke> block must recover the
// call instead of short-circuiting before the scanner runs.
func TestRewriteTextToolCallsBareInvoke(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"<invoke name=\"bash\"><parameter name=\"cmd\">ls</parameter></invoke>"},"finish_reason":"stop"}]}`)
	rewritten := RewriteTextToolCalls(body, Chat, nil)
	var payload map[string]any
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	choice, _ := jsonutil.SliceAt(payload, "choices")[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if content, _ := message["content"].(string); content != "" {
		t.Fatalf("content = %q, want the block removed", content)
	}
	calls := jsonutil.SliceAt(message, "tool_calls")
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v, want one recovered call", calls)
	}
	call, _ := calls[0].(map[string]any)
	function, _ := call["function"].(map[string]any)
	if function["name"] != "bash" {
		t.Fatalf("name = %#v, want bash", function["name"])
	}
	if got := argumentsValue(t, function["arguments"].(string))["cmd"]; got != "ls" {
		t.Fatalf("cmd = %#v, want ls", got)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", choice["finish_reason"])
	}
}

// TestChatStreamRecoveredCallsFollowNativeIndices pins the merge of calls
// recovered from text into a stream that already carried a native tool call:
// the native call must survive, the recovered one must start past its index,
// and the finish reason must become tool_calls.
func TestChatStreamRecoveredCallsFollowNativeIndices(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c0\",\"function\":{\"name\":\"native_tool\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"<tool_calls><tool_call name=\\\"text_tool\\\" arguments='{\\\"cmd\\\":\\\"ls\\\"}'/></tool_calls>\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var out strings.Builder
	if _, err := io.Copy(&out, TextToolStreamReader(strings.NewReader(stream), Chat, nil)); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, `"name":"native_tool"`) {
		t.Fatalf("native call dropped:\n%s", body)
	}
	if !strings.Contains(body, `"name":"text_tool"`) {
		t.Fatalf("recovered call missing:\n%s", body)
	}
	if !strings.Contains(body, `"index":1`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("recovered call not merged after the native index:\n%s", body)
	}
	if strings.Contains(body, `<tool_calls>`) {
		t.Fatalf("tool-call text leaked to the client:\n%s", body)
	}
}

// TestTranscribeChatToResponsesCustomTool pins the Responses custom-tool
// restore over the bridge: a client-declared custom tool that the upstream
// answered with a plain function call comes back as custom_tool_call items,
// with the bridged one-parameter arguments unwrapped to freeform input.
func TestTranscribeChatToResponsesCustomTool(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl_9\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"run\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	_, _, err := TranscodeStreamWithOptions(t.Context(), rec, strings.NewReader(stream), Chat, Responses, "m",
		ResponseOptions{CustomTools: map[string]bool{"shell": true}})
	if err != nil {
		t.Fatalf("TranscodeStreamWithOptions: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.output_item.added") {
		t.Fatalf("no output item:\n%s", body)
	}
	if strings.Count(body, `"type":"custom_tool_call"`) < 2 {
		t.Fatalf("custom tool call not restored on every output shape:\n%s", body)
	}
	if strings.Contains(body, `"type":"function_call"`) {
		t.Fatalf("bridged function shape leaked to a custom-tool client:\n%s", body)
	}
	if !strings.Contains(body, "response.custom_tool_call_input.done") {
		t.Fatalf("custom input done event missing:\n%s", body)
	}
}

// TestCollapseResponsesCustomToolCall pins the Custom flag across the
// collapse path: a Responses custom_tool_call stream folded into a response
// body and converted to Chat keeps its freeform input as the bridged
// one-parameter arguments.
func TestCollapseResponsesCustomToolCall(t *testing.T) {
	stream := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"custom_tool_call\",\"call_id\":\"c1\",\"name\":\"shell\",\"input\":\"\"}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"fc_1\",\"type\":\"custom_tool_call\",\"call_id\":\"c1\",\"name\":\"shell\",\"input\":\"ls\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"
	collapsed, err := CollapseStream(strings.NewReader(stream), Responses, "m")
	if err != nil {
		t.Fatalf("CollapseStream: %v", err)
	}
	converted, err := ConvertResponseWithOptions(Responses, Chat, collapsed, ResponseOptions{})
	if err != nil {
		t.Fatalf("ConvertResponseWithOptions: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatalf("converted body is not JSON: %v", err)
	}
	calls := jsonutil.SliceAt(jsonutil.MapAt(jsonutil.SliceAt(payload, "choices")[0].(map[string]any), "message"), "tool_calls")
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v", calls)
	}
	call := calls[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["name"] != "shell" {
		t.Fatalf("name = %#v, want shell", function["name"])
	}
	if function["arguments"] != `{"input":"ls"}` {
		t.Fatalf("arguments = %#v, want the wrapped freeform input", function["arguments"])
	}
}
