package gateway

import (
	"testing"

	wire "opencode2api/internal/protocol"
)

func TestSanitizeToolHistoryDropsCustomToolOutputOrphan(t *testing.T) {
	payload := map[string]any{
		"input": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "c1", "name": "unlisted_tool", "input": "x"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": "y"},
		},
	}
	sanitizeToolHistory(payload, wire.Responses, map[string]string{}, wire.NewToolRenames(), map[string]bool{})
	for _, raw := range payload["input"].([]any) {
		item, _ := raw.(map[string]any)
		if item["type"] == "custom_tool_call_output" {
			t.Fatalf("orphaned custom_tool_call_output survived sanitizing: %#v", item)
		}
	}
}
