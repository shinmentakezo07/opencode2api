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

	// Other spellings models use for the same dialect. The parser folds them
	// onto the canonical tags before parsing.
	textToolAliasOpen      = "<function_call"
	textToolAliasRootOpen  = "<function_calls"
	textToolAliasClose     = "</function_call>"
	textToolAliasRootClose = "</function_calls>"

	// textToolBufferLimit bounds how much an unfinished block may hold back. A
	// longer candidate is flushed as plain text instead of stalling the stream.
	textToolBufferLimit = 1 << 20
)

// textToolNormalizer folds the alias spellings, including Claude's
// <function_calls><invoke name="…"> form, onto the canonical tags. Root
// spellings come first so the longer tag wins at a shared prefix.
var textToolNormalizer = strings.NewReplacer(
	textToolAliasRootOpen, textToolRootOpen,
	textToolAliasRootClose, textToolRootClose,
	textToolAliasOpen, textToolOpen,
	textToolAliasClose, textToolClose,
	"<invoke", textToolOpen,
	"</invoke>", textToolClose,
)

// normalizeTextToolTags folds alias tags onto the canonical ones. Only a
// function_call block is rewritten, so <invoke> in ordinary tool-call text is
// left alone.
func normalizeTextToolTags(text string) string {
	if !strings.Contains(text, "function_call") {
		return text
	}
	return textToolNormalizer.Replace(text)
}

// indexTextToolOpen returns the earliest canonical or alias opener, or -1.
func indexTextToolOpen(text string) int {
	best := -1
	for _, marker := range []string{textToolOpen, textToolAliasOpen} {
		if index := strings.Index(text, marker); index >= 0 && (best < 0 || index < best) {
			best = index
		}
	}
	return best
}

// textToolCloseTag returns the closing tag that matches the opener at the start
// of text.
func textToolCloseTag(text string) string {
	switch {
	case strings.HasPrefix(text, textToolRootOpen):
		return textToolRootClose
	case strings.HasPrefix(text, textToolAliasRootOpen):
		return textToolAliasRootClose
	case strings.HasPrefix(text, textToolAliasOpen):
		return textToolAliasClose
	default:
		return textToolClose
	}
}

// containsTextToolOpen reports whether text carries any opener spelling.
func containsTextToolOpen(text string) bool {
	return strings.Contains(text, textToolOpen) || strings.Contains(text, textToolAliasOpen)
}

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
			index := indexTextToolOpen(s.pending)
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
		// A self-closing <tool_call name="…" arguments="…"/> carries the whole
		// call in its opening tag and has no body or closing tag.
		if opening := strings.IndexByte(s.pending, '>'); opening > 0 && s.pending[opening-1] == '/' {
			block := s.pending[:opening+1]
			s.pending = s.pending[opening+1:]
			s.holding = false
			if call, ok := parseSelfClosingTextToolCall(block, s.renames); ok {
				calls = append(calls, call)
			} else {
				out.WriteString(block)
			}
			continue
		}
		closeTag := textToolCloseTag(s.pending)
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
	best := 0
	for _, marker := range []string{textToolRootOpen, textToolAliasRootOpen} {
		limit := len(marker) - 1
		if limit > len(text) {
			limit = len(text)
		}
		for length := limit; length > 0; length-- {
			if strings.HasSuffix(text, marker[:length]) {
				if length > best {
					best = length
				}
				break
			}
		}
	}
	return best
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
	block = normalizeTextToolTags(block)
	inner := block
	nameAttribute := ""
	isRoot := strings.HasPrefix(inner, textToolRootOpen)
	if isRoot {
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
	if len(calls) == 0 && isRoot {
		// Some models put the JSON call (or an array of them) straight in the
		// <tool_calls> wrapper without a <tool_call> element.
		if parsed, ok := parseTextJSONCalls(inner, renames); ok {
			calls = parsed
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
	needle := strings.ToLower(name) + "="
	lower := strings.ToLower(tag)
	for offset := 0; offset < len(lower); {
		index := strings.Index(lower[offset:], needle)
		if index < 0 {
			return ""
		}
		index += offset
		offset = index + len(needle)
		// data-name= is a different attribute from name=.
		if index > 0 && !isTagSeparator(tag[index-1]) {
			continue
		}
		value := strings.TrimSpace(tag[offset:])
		if value == "" {
			continue
		}
		quote := value[0]
		if quote != '"' && quote != '\'' {
			continue
		}
		value = value[1:]
		if end := strings.IndexByte(value, quote); end >= 0 {
			return value[:end]
		}
	}
	return ""
}

// isTagSeparator reports whether char can precede an attribute name in a tag.
func isTagSeparator(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
}

// parseOneTextToolCall parses the body of one <tool_call>. A JSON object
// supplies name and arguments directly; otherwise the first child tag names
// the tool and its child tags are the parameters.
func parseOneTextToolCall(chunk, nameAttribute string, renames *ToolRenames) (textToolCall, bool) {
	body := strings.TrimSpace(chunk)
	if strings.HasPrefix(body, "{") {
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err == nil {
			return parseTextJSONCall(payload, nameAttribute, renames)
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

// parseTextJSONCall turns one decoded JSON object into a call. The tool name
// comes from the object, or from the tag when <tool_call name="…">{…}</tool_call>
// carries it there; in that form the object itself is the argument map.
func parseTextJSONCall(payload map[string]any, nameAttribute string, renames *ToolRenames) (textToolCall, bool) {
	name := firstTextAny(payload, "name", "tool", "tool_name", "function")
	if name == "" {
		name = nameAttribute
	}
	if name == "" {
		return textToolCall{}, false
	}
	arguments := "{}"
	if raw, ok := firstTextValue(payload, "arguments", "parameters", "params", "input", "args"); ok {
		arguments = textArguments(raw)
	} else if nested, ok := payload["function"].(map[string]any); ok {
		// Legacy OpenAI shape: {"function":{"name":"…","arguments":{…}}}.
		// The name helper already reads function.name, so read its arguments
		// from the same object instead of dropping them.
		if raw, ok := firstTextValue(nested, "arguments", "parameters", "params", "input", "args"); ok {
			arguments = textArguments(raw)
		} else if nameAttribute != "" {
			arguments = textArguments(nested)
		}
	} else if nameAttribute != "" {
		arguments = textArguments(payload)
	}
	name = resolveTextToolName(name, renames)
	if name == "" {
		return textToolCall{}, false
	}
	return textToolCall{Name: name, Arguments: arguments}, true
}

// parseTextJSONCalls reads the body of a <tool_calls> wrapper that holds one
// JSON call or an array of them, without any <tool_call> element.
func parseTextJSONCalls(text string, renames *ToolRenames) ([]textToolCall, bool) {
	body := strings.TrimSpace(text)
	if body == "" {
		return nil, false
	}
	if strings.HasPrefix(body, "[") {
		var list []map[string]any
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			return nil, false
		}
		var calls []textToolCall
		for _, payload := range list {
			if call, ok := parseTextJSONCall(payload, "", renames); ok {
				calls = append(calls, call)
			}
		}
		if len(calls) == 0 {
			return nil, false
		}
		return calls, true
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return nil, false
	}
	call, ok := parseTextJSONCall(payload, "", renames)
	if !ok {
		return nil, false
	}
	return []textToolCall{call}, true
}

// parseSelfClosingTextToolCall parses <tool_call name="…" arguments="…"/>,
// where the opening tag carries the whole call.
func parseSelfClosingTextToolCall(tag string, renames *ToolRenames) (textToolCall, bool) {
	tag = normalizeTextToolTags(tag)
	if !strings.HasPrefix(tag, textToolOpen) {
		return textToolCall{}, false
	}
	attributes := strings.TrimSuffix(strings.TrimPrefix(tag, textToolOpen), "/>")
	name := tagAttribute(attributes, "name")
	if name == "" {
		return textToolCall{}, false
	}
	arguments := "{}"
	for _, key := range []string{"arguments", "args", "parameters", "params", "input"} {
		if raw := tagAttribute(attributes, key); raw != "" {
			arguments = textAttributeArguments(raw)
			break
		}
	}
	name = resolveTextToolName(name, renames)
	if name == "" {
		return textToolCall{}, false
	}
	return textToolCall{Name: name, Arguments: arguments}, true
}

// textAttributeArguments renders an attribute value as the arguments string a
// native call carries. JSON is kept; a bare value has no parameter name to
// attach to, so it is carried as "input".
func textAttributeArguments(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "{}"
	}
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		return textArguments(decoded)
	}
	encoded, err := json.Marshal(map[string]any{"input": value})
	if err != nil {
		return "{}"
	}
	return string(encoded)
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
		rawTag := body[opening+1 : opening+1+close]
		trimmedTag := strings.TrimSpace(rawTag)
		selfClosing := strings.HasSuffix(trimmedTag, "/")
		key := trimmedTag
		if cut := strings.IndexAny(key, " \t\r\n"); cut >= 0 {
			key = key[:cut]
		}
		key = strings.TrimSuffix(key, "/")
		tagName := key
		if strings.EqualFold(key, "parameter") {
			// <parameter name="command">ls</parameter> is a common spelling;
			// the parameter name becomes the key but the closing tag stays
			// </parameter>.
			if name := tagAttribute(rawTag, "name"); name != "" {
				key = name
			}
		}
		body = body[opening+1+close+1:]
		if key == "" || strings.HasPrefix(key, "/") {
			// Skip closing tags; only opening tags name a parameter.
			continue
		}
		if selfClosing {
			// <flag/> has no body and no closing tag; record its empty value
			// instead of treating the rest of the call as its value.
			arguments[key] = parseTextValue("")
			continue
		}
		end := strings.Index(body, "</"+tagName+">")
		if end < 0 {
			// An unclosed parameter keeps the rest as its value rather than
			// losing it.
			arguments[key] = parseTextValue(body)
			body = ""
			break
		}
		arguments[key] = parseTextValue(body[:end])
		body = body[end+len(tagName)+3:]
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
	return decodeTextEntities(value)
}

// textEntityReplacer decodes the five predefined XML entities a model may use
// inside a tag-dialect value.
var textEntityReplacer = strings.NewReplacer(
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&apos;", "'",
	"&amp;", "&",
)

// decodeTextEntities decodes XML entities in a raw text value.
func decodeTextEntities(text string) string {
	if !strings.Contains(text, "&") {
		return text
	}
	return textEntityReplacer.Replace(text)
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
	if len(body) == 0 || (!bytes.Contains(body, []byte("tool_call")) && !bytes.Contains(body, []byte("function_call"))) {
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
	if !ok || !containsTextToolOpen(text) {
		return false
	}
	clean, calls := extractTextToolCalls(text, renames)
	if len(calls) == 0 {
		return false
	}
	message["content"] = clean
	var toolCalls []any
	for _, call := range calls {
		toolCalls = append(toolCalls, map[string]any{
			"id":   newToolCallID("call_"),
			"type": "function",
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
		if !ok || !containsTextToolOpen(text) {
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
			if !ok || !containsTextToolOpen(text) {
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
		buffer: bufio.NewReaderSize(reader, 16<<10),
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
			if errors.Is(err, io.EOF) && !reader.done {
				reader.done = true
				if final := reader.stream.finish(); len(final) > 0 {
					// An upstream that ends without a newline leaves the last
					// line unterminated; separate it so the flushed frames do
					// not merge into its data.
					if len(reader.pending) > 0 && !bytes.HasSuffix(reader.pending, []byte("\n\n")) && !bytes.HasSuffix(reader.pending, []byte("\r\n\r\n")) {
						if !bytes.HasSuffix(reader.pending, []byte("\n")) {
							reader.pending = append(reader.pending, '\n')
						}
						reader.pending = append(reader.pending, '\n')
					}
					reader.pending = append(reader.pending, final...)
				}
			}
			if len(reader.pending) == 0 {
				return 0, err
			}
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

	// Anthropic content block bookkeeping. openType records a block the
	// upstream started but has not stopped, so an injected tool block never
	// opens on top of one that is still open.
	blockIndex int
	openIndex  int
	openType   string

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
		if strings.TrimSpace(body) == "" {
			// A blank line ends the event; an event name that never carried
			// data must not prefix the next event.
			stream.held = ""
		}
		return []byte(line)
	}
	raw := strings.TrimSpace(strings.TrimPrefix(body, "data:"))
	held := stream.held
	stream.held = ""
	if raw == "" {
		return stream.withEvent(held, []byte(line))
	}
	if raw == "[DONE]" {
		// Flush held calls before the client's end marker: a client stops
		// reading at [DONE], so a frame after it would never run the tool.
		final := stream.finish()
		return append(final, stream.withEvent(held, []byte(line))...)
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
	if !stream.chatNeedsScan(raw) {
		return []byte(raw)
	}
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
						if clean == "" {
							delete(message, "content")
						} else {
							message["content"] = clean
						}
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
	if !stream.anthropicNeedsScan(raw) {
		return []byte(raw)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return []byte(raw)
	}
	typeName := jsonutil.StringAt(payload, "type")
	if typeName == "" {
		typeName = stream.event
	}
	switch typeName {
	case "content_block_start":
		if index, ok := payload["index"].(float64); ok {
			stream.openIndex = int(index)
			stream.openType = jsonutil.StringAt(payload, "content_block", "type")
			if stream.openIndex > stream.blockIndex {
				stream.blockIndex = stream.openIndex
			}
		}
		return []byte(raw)
	case "content_block_stop":
		if index, ok := payload["index"].(float64); ok {
			if int(index) == stream.openIndex {
				stream.openType = ""
			}
			if int(index) > stream.blockIndex {
				stream.blockIndex = int(index)
			}
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
	if stream.openType != "" {
		// A truncated stream can end while a block is still open. Close it
		// before opening an injected block, and give held text back to it.
		if tail != "" && stream.openType == "text" {
			out = append(out, anthropicEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": stream.openIndex,
				"delta": map[string]any{"type": "text_delta", "text": tail},
			})...)
			tail = ""
		}
		out = append(out, anthropicEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": stream.openIndex,
		})...)
		stream.openType = ""
	}
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
	if !stream.responsesNeedsScan(raw) {
		return []byte(raw)
	}
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
	if tail == "" && len(stream.calls) == 0 {
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
			"status":    "in_progress",
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
		item["status"] = "completed"
		stream.sequence++
		out = append(out, responsesEvent("response.output_item.done", map[string]any{
			"type":            "response.output_item.done",
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

// chatNeedsScan reports whether a Chat frame can change the scanner or carry a
// recovered call. Plain content frames are forwarded without decoding, which
// keeps the per-token path cheap; a frame is decoded once it holds a possible
// marker start, a finish reason, or while the scanner still holds a fragment.
func (stream *textToolStream) chatNeedsScan(raw string) bool {
	if stream.scanner.pending != "" {
		return true
	}
	if hasChatFinishReason(raw) {
		return true
	}
	return strings.Contains(raw, "<") || strings.Contains(raw, `\u003c`)
}

// hasChatFinishReason reports whether a Chat frame carries a non-null
// finish_reason. OpenAI-compatible streams put "finish_reason":null on every
// content delta, so the field name alone is not a terminal signal.
func hasChatFinishReason(raw string) bool {
	const key = `"finish_reason"`
	index := strings.Index(raw, key)
	if index < 0 {
		return false
	}
	rest := strings.TrimLeft(raw[index+len(key):], " \t\r\n")
	if !strings.HasPrefix(rest, ":") {
		return false
	}
	rest = strings.TrimLeft(rest[1:], " \t\r\n")
	return !strings.HasPrefix(rest, "null")
}

// anthropicNeedsScan reports whether an Anthropic frame can change the scanner
// or the content-block bookkeeping. A plain text delta is forwarded without
// decoding unless it holds a possible marker start.
func (stream *textToolStream) anthropicNeedsScan(raw string) bool {
	if stream.scanner.pending != "" {
		return true
	}
	if strings.Contains(raw, "text_delta") {
		return strings.Contains(raw, "<") || strings.Contains(raw, `\u003c`)
	}
	return strings.Contains(raw, "content_block_") ||
		strings.Contains(raw, "message_delta") ||
		strings.Contains(raw, "message_stop")
}

// responsesNeedsScan reports whether a Responses frame can change the scanner
// or the output bookkeeping. A plain output-text delta is forwarded without
// decoding unless it holds a possible marker start.
func (stream *textToolStream) responsesNeedsScan(raw string) bool {
	if stream.scanner.pending != "" {
		return true
	}
	if strings.Contains(raw, "output_text.delta") {
		return strings.Contains(raw, "<") || strings.Contains(raw, `\u003c`)
	}
	return strings.Contains(raw, "response.output_item.") ||
		strings.Contains(raw, "response.completed") ||
		strings.Contains(raw, "response.incomplete") ||
		strings.Contains(raw, "response.done")
}
