package main

import (
	"encoding/json"
	"testing"
)

func TestNormalizeGrokResponsesRequestAdaptsCodexToolsAndHistory(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.6",
		"service_tier":"priority",
		"parallel_tool_calls":true,
		"tools":[
			{"type":"function","name":"lookup","parameters":{"type":"object"}},
			{"type":"custom","name":"apply_patch","format":{"type":"grammar"}},
			{"type":"custom","name":"shell","format":{"type":"grammar"}},
			{"type":"namespace","name":"mcp","tools":[
				{"type":"function","name":"read","parameters":{"type":"object"}},
				{"type":"custom","name":"exec"}
			]}
		],
		"tool_choice":{"type":"function","namespace":"mcp","name":"read"},
		"input":[
			{"type":"custom_tool_call","call_id":"call_shell","name":"shell","input":"pwd"},
			{"type":"custom_tool_call_output","call_id":"call_shell","output":[{"type":"input_text","text":"ok"}]},
			{"type":"function_call","call_id":"call_read","namespace":"mcp","name":"read","arguments":"{}"}
		]
	}`)
	var payload map[string]any
	if errDecode := json.Unmarshal(raw, &payload); errDecode != nil {
		t.Fatal(errDecode)
	}
	refs := collectGrokResponsesToolRefs(raw, modelRoute{Family: "grok-4.6"})
	if refs["shell"].name != "shell" || !refs["shell"].custom {
		t.Fatalf("custom tool ref = %#v", refs["shell"])
	}
	if refs["mcp__read"].namespace != "mcp" || refs["mcp__read"].name != "read" {
		t.Fatalf("namespace tool ref = %#v", refs["mcp__read"])
	}

	if !normalizeGrokResponsesRequest(payload) {
		t.Fatal("request was not normalized")
	}
	if _, exists := payload["service_tier"]; exists {
		t.Fatal("service_tier was retained")
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("tools = %#v", tools)
	}
	wantNames := []string{"lookup", "shell", "mcp__read", "mcp__exec"}
	for index, wantName := range wantNames {
		tool := tools[index].(map[string]any)
		if tool["type"] != "function" || tool["name"] != wantName {
			t.Fatalf("tools[%d] = %#v", index, tool)
		}
		if _, exists := tool["parameters"]; !exists {
			t.Fatalf("tools[%d] has no parameters: %#v", index, tool)
		}
	}
	choice := payload["tool_choice"].(map[string]any)
	if choice["name"] != "mcp__read" || choice["namespace"] != nil {
		t.Fatalf("tool_choice = %#v", choice)
	}
	input := payload["input"].([]any)
	call := input[0].(map[string]any)
	if call["type"] != "function_call" || call["arguments"] != `{"input":"pwd"}` {
		t.Fatalf("custom history call = %#v", call)
	}
	output := input[1].(map[string]any)
	var outputItems []map[string]any
	outputJSON, _ := output["output"].(string)
	if output["type"] != "function_call_output" || json.Unmarshal([]byte(outputJSON), &outputItems) != nil ||
		len(outputItems) != 1 || outputItems[0]["type"] != "input_text" || outputItems[0]["text"] != "ok" {
		t.Fatalf("custom history output = %#v", output)
	}
	namespacedCall := input[2].(map[string]any)
	if namespacedCall["name"] != "mcp__read" || namespacedCall["namespace"] != nil {
		t.Fatalf("namespace history call = %#v", namespacedCall)
	}
}

func TestNormalizeGrokResponsesRequestDropsToolControlsWhenOnlyApplyPatchExists(t *testing.T) {
	payload := map[string]any{
		"service_tier":        "auto",
		"parallel_tool_calls": true,
		"tool_choice":         "required",
		"tools": []any{
			map[string]any{"type": "custom", "name": "apply_patch"},
		},
	}
	normalizeGrokResponsesRequest(payload)
	for _, field := range []string{"service_tier", "tools", "parallel_tool_calls", "tool_choice"} {
		if _, exists := payload[field]; exists {
			t.Fatalf("%s was retained: %#v", field, payload[field])
		}
	}
}

func TestNormalizeGrokResponsesRequestDropsUnsupportedCodexControlsAndTools(t *testing.T) {
	payload := map[string]any{
		"background":         false,
		"context_management": []any{map[string]any{"type": "compaction"}},
		"tools": []any{
			map[string]any{"type": "tool_search"},
			map[string]any{"type": "web_search_preview"},
			map[string]any{"type": "web_search", "external_web_access": true},
			map[string]any{"type": "function", "name": "lookup"},
		},
	}
	if !normalizeGrokResponsesRequest(payload) {
		t.Fatal("request was not normalized")
	}
	for _, field := range []string{"background", "context_management"} {
		if _, exists := payload[field]; exists {
			t.Fatalf("%s was retained: %#v", field, payload[field])
		}
	}
	tools := payload["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	webSearch := tools[0].(map[string]any)
	if webSearch["type"] != "web_search" {
		t.Fatalf("web search tool = %#v", webSearch)
	}
	if _, exists := webSearch["external_web_access"]; exists {
		t.Fatalf("external_web_access was retained: %#v", webSearch)
	}
	tool := tools[1].(map[string]any)
	parameters := tool["parameters"].(map[string]any)
	if tool["name"] != "lookup" || parameters["type"] != "object" {
		t.Fatalf("function tool = %#v", tool)
	}
	if _, exists := parameters["additionalProperties"]; exists {
		t.Fatalf("missing parameters did not use the empty object schema: %#v", parameters)
	}
}

func TestNormalizeGrokResponsesToolsNormalizesAndSimplifiesRootUnionSchemas(t *testing.T) {
	payload := map[string]any{
		"tools": []any{
			map[string]any{
				"type": "function", "name": "crop", "strict": true,
				"parameters": map[string]any{
					"type": "object",
					"oneOf": []any{
						map[string]any{"required": []any{"radius"}},
						map[string]any{"required": []any{"size"}},
					},
					"properties": map[string]any{"radius": map[string]any{"type": "number"}},
				},
			},
			map[string]any{
				"type": "function", "name": "nullable", "strict": true,
				"parameters": map[string]any{
					"anyOf": []any{
						map[string]any{"type": "object"},
						map[string]any{"type": []any{"object", "null"}},
					},
				},
			},
			map[string]any{
				"type": "namespace", "name": "codex_app", "tools": []any{
					map[string]any{
						"type": "function", "name": "automation_update", "strict": true,
						"parameters": map[string]any{"type": "object", "$defs": map[string]any{}},
					},
				},
			},
		},
	}
	if !normalizeGrokResponsesRequest(payload) {
		t.Fatal("request was not normalized")
	}
	tools := payload["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %#v", tools)
	}

	preserved := tools[0].(map[string]any)
	preservedParameters := preserved["parameters"].(map[string]any)
	for index, rawBranch := range preservedParameters["oneOf"].([]any) {
		branch := rawBranch.(map[string]any)
		if branch["type"] != "object" {
			t.Fatalf("oneOf[%d] = %#v", index, branch)
		}
	}
	if preserved["strict"] != true || preservedParameters["properties"] == nil {
		t.Fatalf("object-root schema was not preserved: %#v", preserved)
	}

	for _, index := range []int{1, 2} {
		tool := tools[index].(map[string]any)
		parameters := tool["parameters"].(map[string]any)
		if parameters["type"] != "object" || parameters["additionalProperties"] != true {
			t.Fatalf("tools[%d] parameters = %#v", index, parameters)
		}
		if tool["strict"] != false {
			t.Fatalf("tools[%d] strict = %#v", index, tool["strict"])
		}
	}
	if tools[2].(map[string]any)["name"] != "codex_app__automation_update" {
		t.Fatalf("automation tool = %#v", tools[2])
	}
}

func TestGrokResponsesResponseNormalizerRestoresCustomAndNamespaceCalls(t *testing.T) {
	refs := map[string]grokResponsesToolRef{
		"shell":     {name: "shell", custom: true},
		"mcp__read": {namespace: "mcp", name: "read"},
	}
	normalizer := newGrokResponsesResponseNormalizer(refs)
	raw := []byte(`{
		"id":"resp_1","object":"response","status":"completed",
		"output":[
			{"id":"fc_shell","type":"function_call","call_id":"call_shell","name":"shell","arguments":"{\"input\":\"pwd\"}"},
			{"id":"fc_read","type":"function_call","call_id":"call_read","name":"mcp__read","arguments":"{}"}
		]
	}`)
	out := normalizer.normalizeJSON(raw)
	var response map[string]any
	if errDecode := json.Unmarshal(out, &response); errDecode != nil {
		t.Fatal(errDecode)
	}
	items := response["output"].([]any)
	custom := items[0].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["name"] != "shell" || custom["input"] != "pwd" {
		t.Fatalf("custom response item = %#v", custom)
	}
	if _, exists := custom["arguments"]; exists {
		t.Fatalf("custom response retained arguments: %#v", custom)
	}
	namespaced := items[1].(map[string]any)
	if namespaced["type"] != "function_call" || namespaced["name"] != "read" || namespaced["namespace"] != "mcp" {
		t.Fatalf("namespace response item = %#v", namespaced)
	}
}

func TestGrokResponsesResponseNormalizerRestoresCustomStreamEvents(t *testing.T) {
	normalizer := newGrokResponsesResponseNormalizer(map[string]grokResponsesToolRef{
		"shell": {name: "shell", custom: true},
	})
	events := [][]byte{
		[]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_shell","type":"function_call","call_id":"call_shell","name":"shell","arguments":""}}`),
		[]byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_shell","delta":"{\"input\":\""}`),
		[]byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_shell","delta":"pwd\"}"}`),
		[]byte(`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_shell"}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_shell","type":"function_call","call_id":"call_shell","name":"shell","arguments":"{\"input\":\"pwd\"}"}}`),
	}
	wantTypes := []string{
		"response.output_item.added",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.output_item.done",
	}
	for index, event := range events {
		out := normalizer.normalizeJSON(event)
		var decoded map[string]any
		if errDecode := json.Unmarshal(out, &decoded); errDecode != nil {
			t.Fatal(errDecode)
		}
		if decoded["type"] != wantTypes[index] {
			t.Fatalf("event %d type = %#v; body=%s", index, decoded["type"], out)
		}
		if index == 0 || index == 4 {
			item := decoded["item"].(map[string]any)
			if item["type"] != "custom_tool_call" {
				t.Fatalf("event %d item = %#v", index, item)
			}
		}
		if (index == 1 || index == 2) && decoded["delta"] != "" {
			t.Fatalf("delta event leaked function argument wrapper: %#v", decoded)
		}
		if index == 3 && decoded["input"] != "pwd" {
			t.Fatalf("done event = %#v", decoded)
		}
	}
}

func TestNormalizeOpenAIResponsesLeavesCodexToolsOutsideGrok(t *testing.T) {
	payload := map[string]any{
		"model":        "gpt-5.6-sol",
		"service_tier": "priority",
		"tools": []any{
			map[string]any{"type": "custom", "name": "apply_patch"},
		},
	}
	normalizeOpenAIResponsesCompatibility(payload, modelRoute{Family: "gpt-5.6"}, false)
	if payload["service_tier"] != "priority" || payload["tools"].([]any)[0].(map[string]any)["type"] != "custom" {
		t.Fatalf("non-Grok payload changed: %#v", payload)
	}
}
