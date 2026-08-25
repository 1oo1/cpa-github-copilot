package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

const grokSafeFunctionParametersType = "object"

const (
	grokCodexAppNamespaceName    = "codex_app"
	grokAutomationUpdateToolName = "automation_update"
)

type grokResponsesToolRef struct {
	namespace string
	name      string
	custom    bool
}

// collectGrokResponsesToolRefs records the client-facing identity of tools that
// must be flattened before a Grok Responses request is sent upstream. The map
// is kept with the prepared request so streamed and non-streamed responses can
// be restored before they are returned to the caller.
func collectGrokResponsesToolRefs(raw []byte, route modelRoute) map[string]grokResponsesToolRef {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil || !isGrokResponsesRoute(payload, route) {
		return nil
	}
	refs := make(map[string]grokResponsesToolRef)
	collectGrokResponsesToolRefsFromTools(payload["tools"], refs)
	if input, ok := payload["input"].([]any); ok {
		for _, rawItem := range input {
			item, ok := rawItem.(map[string]any)
			if ok && stringValue(item["type"]) == "additional_tools" {
				collectGrokResponsesToolRefsFromTools(item["tools"], refs)
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

func collectGrokResponsesToolRefsFromTools(raw any, refs map[string]grokResponsesToolRef) {
	tools, ok := raw.([]any)
	if !ok {
		return
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType := stringValue(tool["type"])
		name := strings.TrimSpace(stringValue(tool["name"]))
		switch toolType {
		case "custom":
			if name != "" && name != "apply_patch" {
				refs[name] = grokResponsesToolRef{name: name, custom: true}
			}
		case "namespace":
			if name == "" {
				continue
			}
			nested, _ := tool["tools"].([]any)
			for _, rawNested := range nested {
				nestedTool, ok := rawNested.(map[string]any)
				if !ok {
					continue
				}
				nestedType := stringValue(nestedTool["type"])
				nestedName := strings.TrimSpace(stringValue(nestedTool["name"]))
				qualified := qualifyGrokNamespaceToolName(name, nestedName)
				if qualified == "" || (nestedType == "custom" && nestedName == "apply_patch") {
					continue
				}
				if nestedType == "function" || nestedType == "custom" {
					refs[qualified] = grokResponsesToolRef{
						namespace: name,
						name:      nestedName,
						custom:    nestedType == "custom",
					}
				}
			}
		}
	}
}

// normalizeGrokResponsesRequest adapts Codex Responses extensions to the
// subset accepted by GitHub Copilot's Grok endpoint. This mirrors the proven
// xAI executor behavior in CLIProxyAPI: custom tools become functions,
// apply_patch is omitted, namespace tools are flattened, and matching history
// items are rewritten.
func normalizeGrokResponsesRequest(payload map[string]any) bool {
	changed := false
	for _, field := range []string{"service_tier", "background", "context_management"} {
		if _, exists := payload[field]; exists {
			delete(payload, field)
			changed = true
		}
	}

	if tools, exists := payload["tools"]; exists {
		normalized, toolsChanged := normalizeGrokResponsesTools(tools)
		if toolsChanged {
			changed = true
			if len(normalized) == 0 {
				delete(payload, "tools")
			} else {
				payload["tools"] = normalized
			}
		}
	}
	if promoteGrokResponsesAdditionalTools(payload) {
		changed = true
	}
	if normalizeGrokResponsesToolChoice(payload) {
		changed = true
	}
	if pruneGrokResponsesToolChoice(payload) {
		changed = true
	}
	tools, hasTools := payload["tools"].([]any)
	if !hasTools || len(tools) == 0 {
		delete(payload, "tools")
		if _, exists := payload["tool_choice"]; exists {
			delete(payload, "tool_choice")
			changed = true
		}
		if _, exists := payload["parallel_tool_calls"]; exists {
			delete(payload, "parallel_tool_calls")
			changed = true
		}
	}
	if normalizeGrokResponsesInput(payload) {
		changed = true
	}
	return changed
}

func promoteGrokResponsesAdditionalTools(payload map[string]any) bool {
	input, ok := payload["input"].([]any)
	if !ok {
		return false
	}
	tools, _ := payload["tools"].([]any)
	remaining := make([]any, 0, len(input))
	changed := false
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || stringValue(item["type"]) != "additional_tools" {
			remaining = append(remaining, rawItem)
			continue
		}
		normalized, _ := normalizeGrokResponsesTools(item["tools"])
		tools = append(tools, normalized...)
		changed = true
	}
	if !changed {
		return false
	}
	payload["input"] = remaining
	if len(tools) == 0 {
		delete(payload, "tools")
	} else {
		payload["tools"] = tools
	}
	return true
}

func normalizeGrokResponsesTools(raw any) ([]any, bool) {
	tools, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	normalized := make([]any, 0, len(tools))
	changed := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			normalized = append(normalized, rawTool)
			continue
		}
		if stringValue(tool["type"]) == "namespace" {
			changed = true
			namespace := strings.TrimSpace(stringValue(tool["name"]))
			nested, _ := tool["tools"].([]any)
			for _, rawNested := range nested {
				if normalizedTool, keep, _ := normalizeGrokResponsesTool(rawNested, namespace); keep {
					normalized = append(normalized, normalizedTool)
				}
			}
			continue
		}
		normalizedTool, keep, toolChanged := normalizeGrokResponsesTool(rawTool, "")
		changed = changed || toolChanged
		if keep {
			normalized = append(normalized, normalizedTool)
		}
	}
	return normalized, changed
}

func normalizeGrokResponsesTool(raw any, namespace string) (any, bool, bool) {
	tool, ok := raw.(map[string]any)
	if !ok {
		return raw, true, false
	}
	toolType := stringValue(tool["type"])
	name := strings.TrimSpace(stringValue(tool["name"]))
	if toolType == "custom" && name == "apply_patch" {
		return nil, false, true
	}
	if toolType == "tool_search" || toolType == "web_search_preview" {
		return nil, false, true
	}
	if toolType == "web_search" {
		if _, exists := tool["external_web_access"]; !exists {
			return raw, true, false
		}
		normalized := cloneJSONMap(tool)
		delete(normalized, "external_web_access")
		return normalized, true, true
	}
	if toolType != "function" && toolType != "custom" {
		return raw, true, false
	}

	normalized := cloneJSONMap(tool)
	changed := false
	if normalizeGrokObjectRootUnionBranchTypes(normalized) {
		changed = true
	}
	if toolType == "custom" {
		normalized["type"] = "function"
		changed = true
	}
	if _, exists := normalized["parameters"]; !exists {
		normalized["parameters"] = map[string]any{
			"type":       grokSafeFunctionParametersType,
			"properties": map[string]any{},
		}
		changed = true
	}
	// GitHub Copilot's Grok endpoint requires function parameter roots and all
	// root union branches to resolve exclusively to objects. Codex Desktop's
	// automation tool also carries a large schema that the endpoint rejects.
	if grokFunctionParametersNeedSimplification(normalized, toolType, namespace) {
		normalized["parameters"] = grokSafeFunctionParameters()
		if strict, ok := normalized["strict"].(bool); ok && strict {
			normalized["strict"] = false
		}
		changed = true
	}
	if namespace != "" {
		normalized["name"] = qualifyGrokNamespaceToolName(namespace, name)
		changed = true
	}
	return normalized, true, changed
}

// normalizeGrokObjectRootUnionBranchTypes preserves object-root schema
// semantics while making untyped root anyOf/oneOf branches explicit. Grok
// rejects those branches even though JSON Schema inherits the object root.
func normalizeGrokObjectRootUnionBranchTypes(tool map[string]any) bool {
	parameters, ok := tool["parameters"].(map[string]any)
	if !ok || stringValue(parameters["type"]) != grokSafeFunctionParametersType {
		return false
	}
	changed := false
	for _, unionName := range []string{"anyOf", "oneOf"} {
		branches, ok := parameters[unionName].([]any)
		if !ok {
			continue
		}
		for _, rawBranch := range branches {
			branch, ok := rawBranch.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := branch["type"]; !exists {
				branch["type"] = grokSafeFunctionParametersType
				changed = true
			}
		}
	}
	return changed
}

func grokFunctionParametersNeedSimplification(tool map[string]any, originalType, namespace string) bool {
	name := strings.TrimSpace(stringValue(tool["name"]))
	qualifiedAutomationName := grokCodexAppNamespaceName + "__" + grokAutomationUpdateToolName
	if originalType == "function" && (strings.EqualFold(name, qualifiedAutomationName) ||
		(strings.EqualFold(strings.TrimSpace(namespace), grokCodexAppNamespaceName) &&
			strings.EqualFold(name, grokAutomationUpdateToolName))) {
		return true
	}

	parameters, ok := tool["parameters"].(map[string]any)
	if !ok {
		return false
	}
	for _, unionName := range []string{"anyOf", "oneOf"} {
		branches, ok := parameters[unionName].([]any)
		if !ok {
			continue
		}
		for _, rawBranch := range branches {
			branch, ok := rawBranch.(map[string]any)
			if !ok || !grokSchemaTypeIsObjectOnly(branch["type"]) {
				return true
			}
		}
	}
	return false
}

func grokSchemaTypeIsObjectOnly(raw any) bool {
	switch typed := raw.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), grokSafeFunctionParametersType)
	case []any:
		if len(typed) == 0 {
			return false
		}
		for _, rawType := range typed {
			typeName, ok := rawType.(string)
			if !ok || !strings.EqualFold(strings.TrimSpace(typeName), grokSafeFunctionParametersType) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func grokSafeFunctionParameters() map[string]any {
	return map[string]any{
		"type":                 grokSafeFunctionParametersType,
		"properties":           map[string]any{},
		"additionalProperties": true,
	}
}

func normalizeGrokResponsesToolChoice(payload map[string]any) bool {
	choice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		return false
	}
	changed := normalizeGrokResponsesToolChoiceItem(choice)
	if allowed, ok := choice["tools"].([]any); ok {
		for _, rawTool := range allowed {
			if tool, ok := rawTool.(map[string]any); ok && normalizeGrokResponsesToolChoiceItem(tool) {
				changed = true
			}
		}
	}
	return changed
}

func normalizeGrokResponsesToolChoiceItem(choice map[string]any) bool {
	if stringValue(choice["type"]) != "function" {
		return false
	}
	namespace := strings.TrimSpace(stringValue(choice["namespace"]))
	if namespace == "" {
		return false
	}
	choice["name"] = qualifyGrokNamespaceToolName(namespace, stringValue(choice["name"]))
	delete(choice, "namespace")
	return true
}

func pruneGrokResponsesToolChoice(payload map[string]any) bool {
	choice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		return false
	}
	available := make(map[string]bool)
	if tools, ok := payload["tools"].([]any); ok {
		for _, rawTool := range tools {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			toolType := strings.TrimSpace(stringValue(tool["type"]))
			name := strings.TrimSpace(stringValue(tool["name"]))
			available[grokToolChoiceKey(toolType, name)] = true
		}
	}
	if stringValue(choice["type"]) == "allowed_tools" {
		allowed, ok := choice["tools"].([]any)
		if !ok {
			delete(payload, "tool_choice")
			return true
		}
		filtered := make([]any, 0, len(allowed))
		for _, rawTool := range allowed {
			tool, ok := rawTool.(map[string]any)
			if ok && available[grokToolChoiceKey(stringValue(tool["type"]), stringValue(tool["name"]))] {
				filtered = append(filtered, rawTool)
			}
		}
		if len(filtered) == len(allowed) {
			return false
		}
		if len(filtered) == 0 {
			delete(payload, "tool_choice")
		} else {
			choice["tools"] = filtered
		}
		return true
	}
	if available[grokToolChoiceKey(stringValue(choice["type"]), stringValue(choice["name"]))] {
		return false
	}
	delete(payload, "tool_choice")
	return true
}

func grokToolChoiceKey(toolType, name string) string {
	toolType = strings.TrimSpace(toolType)
	if toolType == "function" || toolType == "custom" {
		return toolType + "\x00" + strings.TrimSpace(name)
	}
	return toolType
}

func normalizeGrokResponsesInput(payload map[string]any) bool {
	input, ok := payload["input"].([]any)
	if !ok {
		return false
	}
	normalized := make([]any, 0, len(input))
	changed := false
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			normalized = append(normalized, rawItem)
			continue
		}
		switch stringValue(item["type"]) {
		case "custom_tool_call":
			callID := strings.TrimSpace(stringValue(item["call_id"]))
			name := strings.TrimSpace(stringValue(item["name"]))
			if callID == "" || name == "" {
				changed = true
				continue
			}
			if namespace := strings.TrimSpace(stringValue(item["namespace"])); namespace != "" {
				name = qualifyGrokNamespaceToolName(namespace, name)
			}
			normalized = append(normalized, map[string]any{
				"type":      "function_call",
				"call_id":   callID,
				"name":      name,
				"arguments": grokCustomToolCallArguments(item["input"]),
			})
			changed = true
		case "custom_tool_call_output":
			callID := strings.TrimSpace(stringValue(item["call_id"]))
			if callID == "" {
				changed = true
				continue
			}
			normalized = append(normalized, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  grokCustomToolCallOutput(item["output"]),
			})
			changed = true
		case "function_call":
			namespace := strings.TrimSpace(stringValue(item["namespace"]))
			if namespace == "" {
				normalized = append(normalized, rawItem)
				continue
			}
			rewritten := cloneJSONMap(item)
			rewritten["name"] = qualifyGrokNamespaceToolName(namespace, stringValue(item["name"]))
			delete(rewritten, "namespace")
			normalized = append(normalized, rewritten)
			changed = true
		default:
			normalized = append(normalized, rawItem)
		}
	}
	if changed {
		payload["input"] = normalized
	}
	return changed
}

func grokCustomToolCallArguments(input any) string {
	if input == nil {
		return "{}"
	}
	if text, ok := input.(string); ok {
		trimmed := strings.TrimSpace(text)
		var object map[string]any
		if json.Unmarshal([]byte(trimmed), &object) == nil && object != nil {
			return trimmed
		}
		encoded, errMarshal := json.Marshal(text)
		if errMarshal != nil {
			return "{}"
		}
		return `{"input":` + string(encoded) + `}`
	}
	if encoded, errMarshal := json.Marshal(input); errMarshal == nil {
		if _, ok := input.(map[string]any); ok {
			return string(encoded)
		}
		return `{"input":` + string(encoded) + `}`
	}
	return "{}"
}

func grokCustomToolCallOutput(output any) string {
	if output == nil {
		return ""
	}
	if text, ok := output.(string); ok {
		return text
	}
	encoded, errMarshal := json.Marshal(output)
	if errMarshal != nil {
		return ""
	}
	return string(encoded)
}

func qualifyGrokNamespaceToolName(namespace, name string) string {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	if namespace == "" || name == "" || strings.HasPrefix(name, "mcp__") {
		return name
	}
	prefix := strings.TrimSuffix(namespace, "__") + "__"
	if strings.HasPrefix(name, prefix) {
		return name
	}
	return prefix + name
}

func cloneJSONMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

type grokResponsesResponseNormalizer struct {
	refs            map[string]grokResponsesToolRef
	itemRefs        map[string]grokResponsesToolRef
	outputRefs      map[string]grokResponsesToolRef
	argumentBuffers map[string]string
}

func newGrokResponsesResponseNormalizer(refs map[string]grokResponsesToolRef) *grokResponsesResponseNormalizer {
	if len(refs) == 0 {
		return nil
	}
	return &grokResponsesResponseNormalizer{
		refs:            refs,
		itemRefs:        make(map[string]grokResponsesToolRef),
		outputRefs:      make(map[string]grokResponsesToolRef),
		argumentBuffers: make(map[string]string),
	}
}

func (n *grokResponsesResponseNormalizer) normalizeJSON(raw []byte) []byte {
	if n == nil || len(raw) == 0 || !json.Valid(raw) {
		return raw
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]any
	if decoder.Decode(&root) != nil || root == nil {
		return raw
	}

	if item, ok := root["item"].(map[string]any); ok {
		if ref, restored := n.restoreItem(item); restored {
			n.rememberItem(root, item, ref)
		}
	}
	n.restoreOutput(root["output"])
	if response, ok := root["response"].(map[string]any); ok {
		n.restoreOutput(response["output"])
	}

	if ref, ok := n.eventRef(root); ok && ref.custom {
		switch stringValue(root["type"]) {
		case "response.function_call_arguments.delta":
			root["type"] = "response.custom_tool_call_input.delta"
			// Function arguments stream as JSON fragments such as
			// {"input":" and pwd"}. Passing those fragments through would
			// corrupt the raw custom-tool input expected by Codex. Buffer them
			// and let the done event carry the decoded complete input.
			if delta, ok := root["delta"].(string); ok {
				if key := n.eventStateKey(root); key != "" {
					n.argumentBuffers[key] += delta
				}
				root["delta"] = ""
			}
		case "response.function_call_arguments.done":
			root["type"] = "response.custom_tool_call_input.done"
			if arguments, exists := root["arguments"]; exists {
				root["input"] = grokFunctionArgumentsToCustomInput(arguments)
				delete(root, "arguments")
			} else if key := n.eventStateKey(root); key != "" {
				root["input"] = grokFunctionArgumentsToCustomInput(n.argumentBuffers[key])
			}
			if key := n.eventStateKey(root); key != "" {
				delete(n.argumentBuffers, key)
			}
		}
	}
	encoded, errMarshal := json.Marshal(root)
	if errMarshal != nil {
		return raw
	}
	return encoded
}

func (n *grokResponsesResponseNormalizer) normalizeSSEFrame(frame []byte) []byte {
	if n == nil {
		return frame
	}
	normalized := normalizeSSEFrame(frame)
	data := sseFrameData(normalized)
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) || !json.Valid(data) {
		return frame
	}
	data = n.normalizeJSON(data)
	return append([]byte("data: "), data...)
}

func (n *grokResponsesResponseNormalizer) restoreOutput(raw any) {
	items, ok := raw.([]any)
	if !ok {
		return
	}
	for index, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if ref, restored := n.restoreItem(item); restored {
			n.outputRefs[strconv.Itoa(index)] = ref
			if id := strings.TrimSpace(stringValue(item["id"])); id != "" {
				n.itemRefs[id] = ref
			}
		}
	}
}

func (n *grokResponsesResponseNormalizer) restoreItem(item map[string]any) (grokResponsesToolRef, bool) {
	if stringValue(item["type"]) != "function_call" {
		return grokResponsesToolRef{}, false
	}
	qualified := strings.TrimSpace(stringValue(item["name"]))
	ref, ok := n.refs[qualified]
	if !ok {
		return grokResponsesToolRef{}, false
	}
	item["name"] = ref.name
	if ref.namespace != "" {
		item["namespace"] = ref.namespace
	}
	if ref.custom {
		item["type"] = "custom_tool_call"
		if arguments, exists := item["arguments"]; exists {
			item["input"] = grokFunctionArgumentsToCustomInput(arguments)
			delete(item, "arguments")
		}
	}
	return ref, true
}

func (n *grokResponsesResponseNormalizer) rememberItem(event, item map[string]any, ref grokResponsesToolRef) {
	for _, key := range []string{"id", "call_id"} {
		if id := strings.TrimSpace(stringValue(item[key])); id != "" {
			n.itemRefs[id] = ref
		}
	}
	if index := jsonScalarKey(event["output_index"]); index != "" {
		n.outputRefs[index] = ref
	}
}

func (n *grokResponsesResponseNormalizer) eventRef(event map[string]any) (grokResponsesToolRef, bool) {
	for _, key := range []string{"item_id", "call_id"} {
		if id := strings.TrimSpace(stringValue(event[key])); id != "" {
			if ref, ok := n.itemRefs[id]; ok {
				return ref, true
			}
		}
	}
	if index := jsonScalarKey(event["output_index"]); index != "" {
		ref, ok := n.outputRefs[index]
		return ref, ok
	}
	return grokResponsesToolRef{}, false
}

func (n *grokResponsesResponseNormalizer) eventStateKey(event map[string]any) string {
	for _, key := range []string{"item_id", "call_id"} {
		if id := strings.TrimSpace(stringValue(event[key])); id != "" {
			return key + ":" + id
		}
	}
	if index := jsonScalarKey(event["output_index"]); index != "" {
		return "output_index:" + index
	}
	return ""
}

func jsonScalarKey(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case float64:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	case string:
		return typed
	default:
		return ""
	}
}

func grokFunctionArgumentsToCustomInput(arguments any) any {
	text, ok := arguments.(string)
	if !ok {
		return arguments
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return text
	}
	if object, ok := decoded.(map[string]any); ok && len(object) == 1 {
		if input, exists := object["input"]; exists {
			if inputText, ok := input.(string); ok {
				return inputText
			}
			if encoded, errMarshal := json.Marshal(input); errMarshal == nil {
				return string(encoded)
			}
		}
	}
	if decodedText, ok := decoded.(string); ok {
		return decodedText
	}
	return text
}
