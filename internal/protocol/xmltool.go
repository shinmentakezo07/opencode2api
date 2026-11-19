package protocol

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"opencode2api/internal/jsonutil"
)

// Models occasionally answer a tool request as text instead of a native call,
// spelling the call in an XML dialect such as
//
//	<tool_calls>
//	<tool_call>
//	<mcp_2fe8dbe32ce3>
//	<commands>
//	"ls -la", "git status"
//	</commands>
//	</mcp_2fe8dbe32ce3>
//	</tool_call>
//	</tool_calls>
//
// A client that only reads native calls then renders that block as ordinary
// assistant text and never runs the tool. The rewrite below recovers the calls
// from the text, removes the block from the visible answer, and emits them in
// the upstream protocol's own tool-call shape before the terminal frame, so
// downstream conversion and the client both see a normal tool call.

const (
	textToolOpen      = "<tool_call"
	textToolRootOpen  = "<tool_calls"
	textToolClose     = "</tool_call>"
	textToolRootClose = "</tool_calls>"

	// textToolBufferLimit bounds how much an unfinished block may hold back. A
	// longer candidate is flushed as plain text instead of stalling the stream.
	textToolBufferLimit = 1 << 20
)

// textToolCall is one call recovered from assistant text. Arguments is the
// JSON object the client receives as the call's arguments string.
type textToolCall struct {
	Name      string
	Arguments string
}

// textToolScanner pulls tool-call blocks out of a text stream. It holds back a
// trailing fragment that may yet become a block opener, and while a block is
// open it holds all text until the closing tag arrives.
type textToolScanner struct {
	pending string
	holding bool
	renames *ToolRenames
}

// push consumes chunk and returns the text that is safe to forward plus every
// call completed by this chunk. With final set, an unfinished block is flushed
// as text instead of being held.
func (s *textToolScanner) push(chunk string, final bool) (string, []textToolCall) {
	s.pending += chunk
	var out strings.Builder
	var calls []textToolCall
	for {
		if !s.holding {
			index := strings.Index(s.pending, textToolOpen)
			if index < 0 {
				keep := heldMarkerPrefix(s.pending)
				if !final {
					out.WriteString(s.pending[:len(s.pending)-keep])
					s.pending = s.pending[len(s.pending)-keep:]
				} else {
					out.WriteString(s.pending)
					s.pending = ""
				}
				break
			}
			if index > 0 {
				out.WriteString(s.pending[:index])
				s.pending = s.pending[index:]
			}
			s.holding = true
		}
		closeTag := textToolClose
		if strings.HasPrefix(s.pending, textToolRootOpen) {
			closeTag = textToolRootClose
		}
		end := strings.Index(s.pending, closeTag)
		if end < 0 {
			if len(s.pending) > textToolBufferLimit || final {
				out.WriteString(s.pending)
				s.pending = ""
				s.holding = false
			}
			break
		}
		end += len(closeTag)
		block := s.pending[:end]
		s.pending = s.pending[end:]
		s.holding = false
		parsed, ok := parseTextToolCalls(block, s.renames)
		if !ok {
			out.WriteString(block)
			continue
		}
		calls = append(calls, parsed...)
	}
	return out.String(), calls
}

// heldMarkerPrefix returns the length of a trailing fragment that could still
// become a block opener, so the scanner does not forward a split marker.
func heldMarkerPrefix(text string) int {
	limit := len(textToolRootOpen) - 1
	if limit > len(text) {
		limit = len(text)
	}
	for length := limit; length > 0; length-- {
		if strings.HasSuffix(text, textToolRootOpen[:length]) {
			return length
		}
	}
	return 0
}

// extractTextToolCalls removes every tool-call block from text and returns the
// remaining text with the calls it carried.
func extractTextToolCalls(text string, renames *ToolRenames) (string, []textToolCall) {
	scanner := &textToolScanner{renames: renames}
	out, calls := scanner.push(text, false)
	tail, more := scanner.push("", true)
	calls = append(calls, more...)
	return out + tail, calls
}

// parseTextToolCalls parses one complete block, either the <tool_calls> wrapper
// or a bare <tool_call>. The inner call is a JSON object or a tag dialect whose
// child tags are the argument names.
func parseTextToolCalls(block string, renames *ToolRenames) ([]textToolCall, bool) {
	inner := block
	nameAttribute := ""
	if strings.HasPrefix(inner, textToolRootOpen) {
		opening := strings.Index(inner, ">")
		if opening < 0 {
			return nil, false
		}
		inner = inner[opening+1:]
		inner = strings.TrimSuffix(inner, textToolRootClose)
	}
	var calls []textToolCall
	rest := inner
	for {
		start := strings.Index(rest, textToolOpen)
		if start < 0 {
			break
		}
		nameAttribute = ""
		rest = rest[start:]
		opening := strings.Index(rest, ">")
		if opening < 0 {
			return nil, false
		}
		nameAttribute = tagAttribute(rest[:opening], "name")
		body := rest[opening+1:]
		end := strings.Index(body, textToolClose)
		if end < 0 {
			return nil, false
		}
		chunk := body[:end]
		rest = body[end+len(textToolClose):]
		call, ok := parseOneTextToolCall(chunk, nameAttribute, renames)
		if ok {
			calls = append(calls, call)
		}
	}
	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
}

// tagAttribute reads an attribute value from an opening tag such as
// `<tool_call name="read">`.
func tagAttribute(tag, name string) string {
	needle := name + "="
	index := strings.Index(strings.ToLower(tag), needle)
	if index < 0 {
		return ""
	}
	value := strings.TrimSpace(tag[index+len(needle):])
	if value == "" {
		return ""
	}
	quote := value[0]
	if quote != '"' && quote != '\'' {
		return ""
	}
	value = value[1:]
	if end := strings.IndexByte(value, quote); end >= 0 {
		return value[:end]
	}
	return ""
}

// parseOneTextToolCall parses the body of one <tool_call>. A JSON object
// supplies name and arguments directly; otherwise the first child tag names
// the tool and its child tags are the parameters.
func parseOneTextToolCall(chunk, nameAttribute string, renames *ToolRenames) (textToolCall, bool) {
	body := strings.TrimSpace(chunk)
	if strings.HasPrefix(body, "{") {
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err == nil {
			name := firstTextAny(payload, "name", "tool", "tool_name", "function")
			if name == "" {
				return textToolCall{}, false
			}
			arguments := "{}"
			if raw, ok := firstTextValue(payload, "arguments", "parameters", "params", "input", "args"); ok {
				arguments = textArguments(raw)
			}
			name = resolveTextToolName(name, renames)
			if name == "" {
				return textToolCall{}, false
			}
			return textToolCall{Name: name, Arguments: arguments}, true
		}
	}
	name := nameAttribute
	params := body
	if name == "" {
		opening := strings.Index(body, "<")
		if opening < 0 {
			return textToolCall{}, false
		}
		close := strings.IndexByte(body[opening+1:], '>')
		if close < 0 {
			return textToolCall{}, false
		}
		name = strings.TrimSpace(body[opening+1 : opening+1+close])
		if cut := strings.IndexAny(name, " \t\r\n"); cut >= 0 {
			name = name[:cut]
		}
		name = strings.Trim(name, "/")
		params = body[opening+1+close+1:]
		params = strings.TrimSuffix(params, "</"+name+">")
	}
	arguments := parseTextTagArguments(params)
	if folded := strings.ToLower(name); folded == "tool_call" || folded == "tool_calls" || folded == "function" {
		return parseOneTextToolCall(params, "", renames)
	}
	name = resolveTextToolName(name, renames)
	if name == "" {
		return textToolCall{}, false
	}
	return textToolCall{Name: name, Arguments: arguments}, true
}

// parseTextTagArguments collects the child tags of a tag-dialect call into a
// JSON object. A call without child tags has empty arguments.
func parseTextTagArguments(body string) string {
	arguments := map[string]any{}
	for {
		opening := strings.Index(body, "<")
		if opening < 0 {
			break
		}
		close := strings.IndexByte(body[opening+1:], '>')
		if close < 0 {
			break
		}
		key := strings.TrimSpace(body[opening+1 : opening+1+close])
		if cut := strings.IndexAny(key, " \t\r\n"); cut >= 0 {
			key = key[:cut]
		}
		key = strings.TrimSuffix(key, "/")
		body = body[opening+1+close+1:]
		if key == "" {
			continue
		}
		end := strings.Index(body, "</"+key+">")
		if end < 0 {
			// An unclosed parameter keeps the rest as its value rather than
			// losing it.
			arguments[key] = parseTextValue(body)
			body = ""
			break
		}
		arguments[key] = parseTextValue(body[:end])
		body = body[end+len(key)+3:]
	}
	if len(arguments) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// parseTextValue decodes a parameter value: JSON when it parses, a packed JSON
// array for the comma-separated form models use for list parameters, and the
// raw text otherwise.
func parseTextValue(raw string) any {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		return decoded
	}
	if strings.Contains(value, `",`) || strings.HasPrefix(value, `"`) {
		if json.Unmarshal([]byte("["+value+"]"), &decoded) == nil {
			return decoded
		}
	}
	return value
}

// textArguments renders a JSON-form call's arguments as the string a native
// call carries.
func textArguments(raw any) string {
	switch value := raw.(type) {
	case nil:
		return "{}"
	case string:
		if strings.TrimSpace(value) == "" {
			return "{}"
		}
		return value
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "{}"
		}
		return string(encoded)
	}
}

// resolveTextToolName maps a name written in text back to the client tool it
// names. The model may drop separators from an opaque wire name, so the fold
// in Resolve matters here.
func resolveTextToolName(name string, renames *ToolRenames) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, "/")
	if renames == nil {
		return name
	}
	client, _ := renames.Resolve(name)
	return client
}

// firstTextAny returns the first non-empty string value among keys.
func firstTextAny(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
		if nested, ok := payload[key].(map[string]any); ok {
			if name, ok := nested["name"].(string); ok && name != "" {
				return name
			}
		}
	}
	return ""
}

// firstTextValue returns the first present value among keys.
func firstTextValue(payload map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			return value, true
		}
	}
	return nil, false
}

// newToolCallID returns a call id with the given protocol prefix.
func newToolCallID(prefix string) string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "000000000000"
	}
	return prefix + hex.EncodeToString(raw[:])
}

// RewriteTextToolCalls converts tool calls a model wrote as assistant text into
// native tool calls inside a complete response body, removing the text from the
// visible answer. A body of another shape, or one without a call block, is
// returned unchanged.
func RewriteTextToolCalls(body []byte, protocol Protocol, renames *ToolRenames) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte("tool_call")) {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	changed := false
	switch protocol {
	case Chat:
		changed = rewriteChatTextTools(payload, renames)
	case Anthropic:
		changed = rewriteAnthropicTextTools(payload, renames)
	case Responses:
		changed = rewriteResponsesTextTools(payload, renames)
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

// rewriteChatTextTools strips text calls out of the first choice's message and
// appends them as tool_calls.
func rewriteChatTextTools(payload map[string]any, renames *ToolRenames) bool {
	choices := jsonutil.SliceAt(payload, "choices")
	if len(choices) == 0 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return false
	}
	text, ok := message["content"].(string)
	if !ok || !strings.Contains(text, textToolOpen) {
		return false
	}
	clean, calls := extractTextToolCalls(text, renames)
	if len(calls) == 0 {
		return false
	}
	message["content"] = clean
	var toolCalls []any
	for index, call := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"index": index,
			"id":    newToolCallID("call_"),
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	existing := jsonutil.SliceAt(message, "tool_calls")
	message["tool_calls"] = append(existing, toolCalls...)
	if reason := jsonutil.StringAt(choice, "finish_reason"); reason == "" || reason == "stop" {
		choice["finish_reason"] = "tool_calls"
	}
	return true
}

// rewriteAnthropicTextTools strips text calls out of text blocks and appends
// tool_use blocks.
func rewriteAnthropicTextTools(payload map[string]any, renames *ToolRenames) bool {
	blocks := jsonutil.SliceAt(payload, "content")
	changed := false
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if jsonutil.StringAt(block, "type") != "text" {
			continue
		}
		text, ok := block["text"].(string)
		if !ok || !strings.Contains(text, textToolOpen) {
			continue
		}
		clean, calls := extractTextToolCalls(text, renames)
		if len(calls) == 0 {
			continue
		}
		block["text"] = clean
		changed = true
		for _, call := range calls {
			input := map[string]any{}
			if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
				input = map[string]any{}
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    newToolCallID("toolu_"),
				"name":  call.Name,
				"input": input,
			})
		}
	}
	if !changed {
		return false
	}
	payload["content"] = blocks
	if reason := jsonutil.StringAt(payload, "stop_reason"); reason == "" || reason == "end_turn" || reason == "stop" {
		payload["stop_reason"] = "tool_use"
	}
	return true
}

// rewriteResponsesTextTools strips text calls out of message output items and
// appends function_call items.
func rewriteResponsesTextTools(payload map[string]any, renames *ToolRenames) bool {
	output := jsonutil.SliceAt(payload, "output")
	changed := false
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if jsonutil.StringAt(item, "type") != "message" {
			continue
		}
		for _, rawPart := range jsonutil.SliceAt(item, "content") {
			part, _ := rawPart.(map[string]any)
			if jsonutil.StringAt(part, "type") != "output_text" {
				continue
			}
			text, ok := part["text"].(string)
			if !ok || !strings.Contains(text, textToolOpen) {
				continue
			}
			clean, calls := extractTextToolCalls(text, renames)
			if len(calls) == 0 {
				continue
			}
			part["text"] = clean
			changed = true
			for _, call := range calls {
				output = append(output, map[string]any{
					"id":        newToolCallID("fc_"),
					"type":      "function_call",
					"status":    "completed",
					"call_id":   newToolCallID("call_"),
					"name":      call.Name,
					"arguments": call.Arguments,
				})
			}
		}
	}
	if !changed {
		return false
	}
	payload["output"] = output
	return true
}

// TextToolStreamReader wraps an upstream SSE stream and turns tool calls the
// model wrote as text into the stream's native tool-call events, holding them
// until the terminal frame so no content block has to be reopened.
func TextToolStreamReader(reader io.Reader, protocol Protocol, renames *ToolRenames) io.Reader {
	return &textToolStreamReader{
		buffer: bufio.NewReaderSize(reader, 64<<10),
		stream: &textToolStream{protocol: protocol, scanner: &textToolScanner{renames: renames}},
	}
}

// textToolStreamReader is the io.Reader TextToolStreamReader returns.
type textToolStreamReader struct {
	buffer  *bufio.Reader
	stream  *textToolStream
	pending []byte
	err     error
	done    bool
}

// Read returns rewritten stream bytes, one source line at a time.
func (reader *textToolStreamReader) Read(data []byte) (int, error) {
	for len(reader.pending) == 0 {
		if reader.err != nil {
			if !reader.done {
				reader.done = true
				reader.pending = reader.stream.finish()
			}
			if len(reader.pending) == 0 {
				return 0, reader.err
			}
			continue
		}
		var lineBytes []byte
		var err error
		for {
			var part []byte
			part, err = reader.buffer.ReadSlice('\n')
			if len(lineBytes)+len(part) > maxSSEFrameBytes {
				reader.err = errSSEFrameTooLarge
				return 0, reader.err
			}
			lineBytes = append(lineBytes, part...)
			if !errors.Is(err, bufio.ErrBufferFull) {
				break
			}
		}
		if len(lineBytes) > 0 {
			reader.pending = reader.stream.rewriteLine(string(lineBytes))
		}
		if err != nil {
			reader.err = err
		}
	}
	n := copy(data, reader.pending)
	reader.pending = reader.pending[n:]
	return n, nil
}

// textToolStream rewrites one protocol's SSE frames.
type textToolStream struct {
	protocol Protocol
	scanner  *textToolScanner
	calls    []textToolCall
	event    string
	held     string

	// Anthropic content block bookkeeping.
	blockIndex int

	// Responses output bookkeeping.
	outputIndex int
	sequence    int
	model       string
	responseID  string
}

// rewriteLine transforms one complete SSE line.
func (stream *textToolStream) rewriteLine(line string) []byte {
	body, terminator := line, ""
	if trimmed := strings.TrimSuffix(line, "\n"); trimmed != line {
		terminator = "\n"
		body = trimmed
		if trimmed = strings.TrimSuffix(body, "\r"); trimmed != body {
			terminator = "\r\n"
			body = trimmed
		}
	}
	if strings.HasPrefix(body, "event:") {
		// Hold the event name until its data line is rewritten: an injected
		// tool block must appear before the terminal message_delta, not after
		// a half-written event.
		stream.event = strings.TrimSpace(strings.TrimPrefix(body, "event:"))
		stream.held = stream.event
		return nil
	}
	if !strings.HasPrefix(body, "data:") {
		return []byte(line)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(body, "data:"))
	held := stream.held
	stream.held = ""
	if raw == "" || raw == "[DONE]" {
		return stream.withEvent(held, []byte(line))
	}
	rewritten := stream.rewriteFrame(raw)
	if string(rewritten) == raw {
		return stream.withEvent(held, []byte(line))
	}
	if bytes.HasPrefix(rewritten, []byte("event: ")) {
		// Injected blocks already carry their own SSE framing.
		return rewritten
	}
	return stream.withEvent(held, []byte("data: "+string(rewritten)+terminator))
}

// withEvent restores a held event line in front of one data line.
func (stream *textToolStream) withEvent(event string, line []byte) []byte {
	if event == "" {
		return line
	}
	return append([]byte("event: "+event+"\n"), line...)
}

// rewriteFrame rewrites one SSE data payload.
func (stream *textToolStream) rewriteFrame(raw string) []byte {
	switch stream.protocol {
	case Chat:
		return stream.rewriteChatFrame(raw)
	case Anthropic:
		return stream.rewriteAnthropicFrame(raw)
	case Responses:
		return stream.rewriteResponsesFrame(raw)
	}
	return []byte(raw)
}

// rewriteChatFrame trims text tool blocks from content deltas and folds the
// recovered calls into the frame that carries finish_reason.
func (stream *textToolStream) rewriteChatFrame(raw string) []byte {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw)
	}
	changed := false
	flush := false
	choices := jsonutil.SliceAt(payload, "choices")
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		if choice != nil {
			message, _ := choice["delta"].(map[string]any)
			if message == nil {
				message, _ = choice["message"].(map[string]any)
			}
			if message != nil {
				if text, ok := message["content"].(string); ok && text != "" {
					clean, calls := stream.scanner.push(text, false)
					if clean != text {
						message["content"] = clean
						changed = true
					}
					if len(calls) > 0 {
						stream.calls = append(stream.calls, calls...)
					}
				}
			}
			if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
				flush = true
			}
		}
	}
	if flush {
		tail, calls := stream.scanner.push("", true)
		stream.calls = append(stream.calls, calls...)
		if tail != "" || len(stream.calls) > 0 {
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["delta"].(map[string]any)
			if message == nil {
				message, _ = choice["message"].(map[string]any)
			}
			if message == nil {
				message = map[string]any{}
				choice["delta"] = message
			}
			if tail != "" {
				existing, _ := message["content"].(string)
				message["content"] = existing + tail
			}
			if len(stream.calls) > 0 {
				message["tool_calls"] = stream.chatToolDeltas()
				choice["finish_reason"] = "tool_calls"
				stream.calls = nil
			}
			changed = true
		}
	}
	if !changed {
		return []byte(raw)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte(raw)
	}
	return encoded
}

// chatToolDeltas renders accumulated calls as Chat delta tool_calls.
func (stream *textToolStream) chatToolDeltas() []any {
	calls := make([]any, 0, len(stream.calls))
	for index, call := range stream.calls {
		calls = append(calls, map[string]any{
			"index": index,
			"id":    newToolCallID("call_"),
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return calls
}

// rewriteAnthropicFrame trims text tool blocks from text deltas and emits
// tool_use blocks just before the message ends.
func (stream *textToolStream) rewriteAnthropicFrame(raw string) []byte {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw)
	}
	typeName := jsonutil.StringAt(payload, "type")
	if typeName == "" {
		typeName = stream.event
	}
	switch typeName {
	case "content_block_start", "content_block_stop":
		if index, ok := payload["index"].(float64); ok && int(index) > stream.blockIndex {
			stream.blockIndex = int(index)
		}
		return []byte(raw)
	case "content_block_delta":
		if index, ok := payload["index"].(float64); ok && int(index) > stream.blockIndex {
			stream.blockIndex = int(index)
		}
		delta, _ := payload["delta"].(map[string]any)
		if jsonutil.StringAt(delta, "type") != "text_delta" {
			return []byte(raw)
		}
		text, ok := delta["text"].(string)
		if !ok || text == "" {
			return []byte(raw)
		}
		clean, calls := stream.scanner.push(text, false)
		if len(calls) > 0 {
			stream.calls = append(stream.calls, calls...)
		}
		if clean == text {
			return []byte(raw)
		}
		delta["text"] = clean
		encoded, err := json.Marshal(payload)
		if err != nil {
			return []byte(raw)
		}
		return encoded
	case "message_delta", "message_stop":
		prefix := stream.anthropicFinish()
		if len(prefix) == 0 {
			return []byte(raw)
		}
		if typeName == "message_delta" {
			if delta, ok := payload["delta"].(map[string]any); ok {
				if reason := jsonutil.StringAt(delta, "stop_reason"); reason == "" || reason == "end_turn" || reason == "stop" {
					delta["stop_reason"] = "tool_use"
				}
			}
		}
		return append(prefix, anthropicEvent(typeName, payload)...)
	}
	return []byte(raw)
}

// anthropicFinish emits any held text and then one tool_use block per call. It
// returns no bytes when nothing is held.
func (stream *textToolStream) anthropicFinish() []byte {
	tail, calls := stream.scanner.push("", true)
	stream.calls = append(stream.calls, calls...)
	if tail == "" && len(stream.calls) == 0 {
		return nil
	}
	var out []byte
	if tail != "" {
		stream.blockIndex++
		index := stream.blockIndex
		out = append(out, anthropicEvent("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         index,
			"content_block": map[string]any{"type": "text", "text": ""},
		})...)
		out = append(out, anthropicEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]any{"type": "text_delta", "text": tail},
		})...)
		out = append(out, anthropicEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		})...)
	}
	for _, call := range stream.calls {
		stream.blockIndex++
		index := stream.blockIndex
		out = append(out, anthropicEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    newToolCallID("toolu_"),
				"name":  call.Name,
				"input": map[string]any{},
			},
		})...)
		out = append(out, anthropicEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
		})...)
		out = append(out, anthropicEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		})...)
	}
	stream.calls = nil
	return out
}

// rewriteResponsesFrame trims text tool blocks from output_text deltas and
// emits function_call items before the response completes.
func (stream *textToolStream) rewriteResponsesFrame(raw string) []byte {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw)
	}
	typeName := jsonutil.StringAt(payload, "type")
	if typeName == "" {
		typeName = stream.event
	}
	if id := jsonutil.StringAt(payload, "response", "id"); id != "" {
		stream.responseID = id
	}
	if model := jsonutil.StringAt(payload, "response", "model"); model != "" {
		stream.model = model
	}
	if number, ok := payload["sequence_number"].(float64); ok && int(number) > stream.sequence {
		stream.sequence = int(number)
	}
	switch typeName {
	case "response.output_item.added", "response.output_item.done":
		if index, ok := payload["output_index"].(float64); ok && int(index) > stream.outputIndex {
			stream.outputIndex = int(index)
		}
		return []byte(raw)
	case "response.output_text.delta":
		if index, ok := payload["output_index"].(float64); ok && int(index) > stream.outputIndex {
			stream.outputIndex = int(index)
		}
		text, ok := payload["delta"].(string)
		if !ok || text == "" {
			return []byte(raw)
		}
		clean, calls := stream.scanner.push(text, false)
		if len(calls) > 0 {
			stream.calls = append(stream.calls, calls...)
		}
		if clean == text {
			return []byte(raw)
		}
		payload["delta"] = clean
		encoded, err := json.Marshal(payload)
		if err != nil {
			return []byte(raw)
		}
		return encoded
	case "response.completed", "response.incomplete", "response.done":
		tail, calls := stream.scanner.push("", true)
		stream.calls = append(stream.calls, calls...)
		if tail == "" && len(stream.calls) == 0 {
			return []byte(raw)
		}
		var prefix []byte
		if tail != "" {
			stream.sequence++
			prefix = append(prefix, responsesEvent("response.output_text.delta", map[string]any{
				"type":            "response.output_text.delta",
				"delta":           tail,
				"output_index":    stream.outputIndex,
				"sequence_number": stream.sequence,
			})...)
		}
		response, _ := payload["response"].(map[string]any)
		if response == nil {
			response = map[string]any{}
			payload["response"] = response
		}
		output := jsonutil.SliceAt(response, "output")
		for _, call := range stream.calls {
			stream.outputIndex++
			index := stream.outputIndex
			itemID := newToolCallID("fc_")
			callID := newToolCallID("call_")
			stream.sequence++
			prefix = append(prefix, responsesEvent("response.output_item.added", map[string]any{
				"type":            "response.output_item.added",
				"output_index":    index,
				"sequence_number": stream.sequence,
				"item": map[string]any{
					"id":        itemID,
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   callID,
					"name":      call.Name,
					"arguments": "",
				},
			})...)
			stream.sequence++
			prefix = append(prefix, responsesEvent("response.function_call_arguments.delta", map[string]any{
				"type":            "response.function_call_arguments.delta",
				"delta":           call.Arguments,
				"item_id":         itemID,
				"output_index":    index,
				"sequence_number": stream.sequence,
			})...)
			stream.sequence++
			prefix = append(prefix, responsesEvent("response.function_call_arguments.done", map[string]any{
				"type":            "response.function_call_arguments.done",
				"arguments":       call.Arguments,
				"item_id":         itemID,
				"output_index":    index,
				"sequence_number": stream.sequence,
			})...)
			stream.sequence++
			item := map[string]any{
				"id":        itemID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   callID,
				"name":      call.Name,
				"arguments": call.Arguments,
			}
			prefix = append(prefix, responsesEvent("response.output_item.done", map[string]any{
				"type":            "response.output_item.done",
				"output_index":    index,
				"sequence_number": stream.sequence,
				"item":            item,
			})...)
			output = append(output, item)
		}
		stream.calls = nil
		response["output"] = output
		stream.sequence++
		payload["sequence_number"] = stream.sequence
		return append(prefix, responsesEvent(typeName, payload)...)
	default:
		return []byte(raw)
	}
}

// finish flushes calls that never reached a terminal frame. The calls become a
// bare tool-call frame of the protocol so a truncated stream still runs them.
func (stream *textToolStream) finish() []byte {
	switch stream.protocol {
	case Chat:
		tail, calls := stream.scanner.push("", true)
		stream.calls = append(stream.calls, calls...)
		if tail == "" && len(stream.calls) == 0 {
			return nil
		}
		var out []byte
		if tail != "" {
			encoded, err := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"index":         0,
					"delta":         map[string]any{"content": tail},
					"finish_reason": nil,
				}},
			})
			if err == nil {
				out = append(out, sseData(encoded)...)
			}
		}
		if len(stream.calls) > 0 {
			encoded, err := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"index":         0,
					"delta":         map[string]any{"tool_calls": stream.chatToolDeltas()},
					"finish_reason": "tool_calls",
				}},
			})
			if err == nil {
				out = append(out, sseData(encoded)...)
			}
			stream.calls = nil
		}
		return out
	case Anthropic:
		return stream.anthropicFinish()
	case Responses:
		prefix := stream.responsesFinish()
		return prefix
	}
	return nil
}

// responsesFinish emits held Responses calls as their own output items. It is
// used only for a stream that ends without a terminal frame.
func (stream *textToolStream) responsesFinish() []byte {
	tail, calls := stream.scanner.push("", true)
	stream.calls = append(stream.calls, calls...)
	if len(stream.calls) == 0 {
		return nil
	}
	var out []byte
	if tail != "" {
		stream.sequence++
		out = append(out, responsesEvent("response.output_text.delta", map[string]any{
			"type":            "response.output_text.delta",
			"delta":           tail,
			"output_index":    stream.outputIndex,
			"sequence_number": stream.sequence,
		})...)
	}
	for _, call := range stream.calls {
		stream.outputIndex++
		index := stream.outputIndex
		item := map[string]any{
			"id":        newToolCallID("fc_"),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   newToolCallID("call_"),
			"name":      call.Name,
			"arguments": call.Arguments,
		}
		stream.sequence++
		out = append(out, responsesEvent("response.output_item.added", map[string]any{
			"type":            "response.output_item.added",
			"output_index":    index,
			"sequence_number": stream.sequence,
			"item":            item,
		})...)
	}
	stream.calls = nil
	return out
}

// anthropicEvent renders one Anthropic SSE event with its event name.
func anthropicEvent(name string, payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []byte("event: " + name + "\ndata: " + string(encoded) + "\n\n")
}

// responsesEvent renders one Responses SSE event with its event name.
func responsesEvent(name string, payload map[string]any) []byte {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return []byte("event: " + name + "\ndata: " + string(encoded) + "\n\n")
}

// sseData renders one SSE data frame.
func sseData(encoded []byte) []byte {
	return []byte("data: " + string(encoded) + "\n\n")
}
