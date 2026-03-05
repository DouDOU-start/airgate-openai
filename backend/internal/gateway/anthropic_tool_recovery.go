package gateway

import (
	"encoding/json"
	"regexp"
	"strings"
)

type recoveredToolCall struct {
	Name      string
	Arguments map[string]any
	Source    string
}

type toolCallSchema struct {
	Name     string
	Required []string
}

var (
	qwenFunctionTagPattern = regexp.MustCompile(`(?is)<function=([^>]+)>`)
	qwenParamTagPattern    = regexp.MustCompile(`(?is)<parameter=([^>]+)>`)

	xmlToolCallPattern  = regexp.MustCompile(`(?is)<tool_call>\s*(\{[\s\S]*?\})\s*</tool_call>`)
	jsonToolCallPattern = regexp.MustCompile(
		`(?is)\{\s*"name"\s*:\s*"([^"]+)"\s*,\s*"(?:arguments|input|parameters)"\s*:\s*(\{[\s\S]*?\})\s*\}`,
	)
	jsonToolInputPattern = regexp.MustCompile(
		`(?is)\{\s*"tool"\s*:\s*"([^"]+)"\s*,\s*"tool_input"\s*:\s*(\{[\s\S]*?\})\s*\}`,
	)
	anthropicToolUsePattern = regexp.MustCompile(
		`(?is)\{\s*"type"\s*:\s*"tool_use"\s*,[\s\S]*?"name"\s*:\s*"([^"]+)"\s*,[\s\S]*?"input"\s*:\s*(\{[\s\S]*?\})\s*\}`,
	)
	openAIToolCallPattern = regexp.MustCompile(
		`(?is)\{\s*"type"\s*:\s*"tool_call"\s*,[\s\S]*?"(?:tool_call|function)"\s*:\s*\{\s*"name"\s*:\s*"([^"]+)"\s*,\s*"arguments"\s*:\s*(\{[\s\S]*?\})\s*\}\s*\}`,
	)

	intentPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?is)(?:i(?:'ll| will| am going to)|let me|going to)\s+([^.!?\n]{12,})`),
		regexp.MustCompile(`(?is)(?:help you|assist with)\s+([^.!?\n]{12,})`),
		regexp.MustCompile(`(?is)(?:explore|search|find|look for|investigate)\s+([^.!?\n]{12,})`),
		regexp.MustCompile(`(?is)(?:implement|create|build|add|fix|update)\s+([^.!?\n]{12,})`),
	}
)

func buildToolCallSchemas(tools []AnthropicTool) map[string]toolCallSchema {
	schemas := make(map[string]toolCallSchema)
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}

		schema := toolCallSchema{Name: name}
		if len(tool.InputSchema) > 0 {
			var parsed struct {
				Required []string `json:"required"`
			}
			if err := json.Unmarshal(tool.InputSchema, &parsed); err == nil {
				schema.Required = append(schema.Required, parsed.Required...)
			}
		}

		schemas[strings.ToLower(name)] = schema
	}
	return schemas
}

func extractToolCallsFromText(text string) []recoveredToolCall {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	var extracted []recoveredToolCall

	// Pattern 0: Qwen style <function=NAME><parameter=key>value
	functionMatches := qwenFunctionTagPattern.FindAllStringSubmatchIndex(text, -1)
	for i, fm := range functionMatches {
		if len(fm) < 4 {
			continue
		}
		name := strings.TrimSpace(text[fm[2]:fm[3]])
		if name == "" {
			continue
		}

		chunkStart := fm[1]
		chunkEnd := len(text)
		if i+1 < len(functionMatches) {
			chunkEnd = functionMatches[i+1][0]
		}
		if chunkStart > chunkEnd || chunkStart < 0 || chunkEnd > len(text) {
			continue
		}
		chunk := text[chunkStart:chunkEnd]

		args := map[string]any{}
		paramMatches := qwenParamTagPattern.FindAllStringSubmatchIndex(chunk, -1)
		for j, pm := range paramMatches {
			if len(pm) < 4 {
				continue
			}
			paramName := strings.TrimSpace(chunk[pm[2]:pm[3]])
			if paramName == "" {
				continue
			}
			valueStart := pm[1]
			valueEnd := len(chunk)
			if j+1 < len(paramMatches) {
				valueEnd = paramMatches[j+1][0]
			}
			if valueStart > valueEnd || valueStart < 0 || valueEnd > len(chunk) {
				continue
			}
			args[paramName] = strings.TrimSpace(chunk[valueStart:valueEnd])
		}

		extracted = append(extracted, recoveredToolCall{
			Name:      name,
			Arguments: args,
			Source:    "xml_text",
		})
	}

	// Pattern 1: <tool_call>{...}</tool_call>
	for _, m := range xmlToolCallPattern.FindAllStringSubmatch(text, -1) {
		name, args, ok := parseToolCallObject(m[1])
		if !ok {
			continue
		}
		extracted = append(extracted, recoveredToolCall{
			Name:      name,
			Arguments: args,
			Source:    "xml_text",
		})
	}

	// Pattern 2: {"name":"X","arguments":{...}}
	for _, m := range jsonToolCallPattern.FindAllStringSubmatch(text, -1) {
		if args, ok := parseJSONObject(m[2]); ok {
			extracted = append(extracted, recoveredToolCall{
				Name:      strings.TrimSpace(m[1]),
				Arguments: args,
				Source:    "json_text",
			})
		}
	}

	// Pattern 2b: {"tool":"X","tool_input":{...}}
	for _, m := range jsonToolInputPattern.FindAllStringSubmatch(text, -1) {
		if args, ok := parseJSONObject(m[2]); ok {
			extracted = append(extracted, recoveredToolCall{
				Name:      strings.TrimSpace(m[1]),
				Arguments: args,
				Source:    "json_text",
			})
		}
	}

	// Pattern 3: {"type":"tool_use","name":"X","input":{...}}
	for _, m := range anthropicToolUsePattern.FindAllStringSubmatch(text, -1) {
		if args, ok := parseJSONObject(m[2]); ok {
			extracted = append(extracted, recoveredToolCall{
				Name:      strings.TrimSpace(m[1]),
				Arguments: args,
				Source:    "json_text",
			})
		}
	}

	// Pattern 3b: {"type":"tool_call","tool_call":{"name":"X","arguments":{...}}}
	for _, m := range openAIToolCallPattern.FindAllStringSubmatch(text, -1) {
		if args, ok := parseJSONObject(m[2]); ok {
			extracted = append(extracted, recoveredToolCall{
				Name:      strings.TrimSpace(m[1]),
				Arguments: args,
				Source:    "json_text",
			})
		}
	}

	return dedupeRecoveredToolCalls(extracted)
}

func dedupeRecoveredToolCalls(calls []recoveredToolCall) []recoveredToolCall {
	if len(calls) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	result := make([]recoveredToolCall, 0, len(calls))
	for _, c := range calls {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		argsJSON, err := json.Marshal(c.Arguments)
		if err != nil {
			argsJSON = []byte("{}")
		}
		key := strings.ToLower(name) + "|" + string(argsJSON)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		c.Name = name
		result = append(result, c)
	}
	return result
}

func parseToolCallObject(raw string) (name string, args map[string]any, ok bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", nil, false
	}

	name = strings.TrimSpace(getStringAny(payload["name"]))
	if name == "" {
		name = strings.TrimSpace(getStringAny(payload["tool"]))
	}
	if name == "" {
		return "", nil, false
	}

	args = normalizeToolArgs(payload["arguments"])
	if len(args) == 0 {
		args = normalizeToolArgs(payload["input"])
	}
	if len(args) == 0 {
		args = normalizeToolArgs(payload["parameters"])
	}
	if len(args) == 0 {
		args = normalizeToolArgs(payload["tool_input"])
	}
	if args == nil {
		args = map[string]any{}
	}
	return name, args, true
}

func parseJSONObject(raw string) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, false
	}
	return obj, true
}

func normalizeToolArgs(v any) map[string]any {
	switch typed := v.(type) {
	case map[string]any:
		return cloneMap(typed)
	case string:
		var decoded map[string]any
		if err := json.Unmarshal([]byte(typed), &decoded); err == nil {
			return decoded
		}
	}
	return nil
}

func validateAndRepairRecoveredToolCall(
	call recoveredToolCall,
	schemas map[string]toolCallSchema,
	contextText string,
) (map[string]any, string, bool) {
	args := cloneMap(call.Arguments)
	if args == nil {
		args = map[string]any{}
	}

	if len(schemas) == 0 {
		return args, call.Name, true
	}

	schema, ok := schemas[strings.ToLower(call.Name)]
	if !ok {
		// When schema exists, only keep declared tools.
		return nil, "", false
	}

	missing := missingRequiredParams(schema.Required, args)
	if len(missing) == 0 {
		return args, schema.Name, true
	}

	repaired := inferMissingToolParams(schema.Name, args, missing, contextText)
	missing = missingRequiredParams(schema.Required, repaired)
	if len(missing) > 0 {
		return nil, "", false
	}
	return repaired, schema.Name, true
}

func missingRequiredParams(required []string, args map[string]any) []string {
	if len(required) == 0 {
		return nil
	}
	var missing []string
	for _, key := range required {
		if valueMissing(args[key]) {
			missing = append(missing, key)
		}
	}
	return missing
}

func valueMissing(v any) bool {
	switch typed := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	}
	return false
}

func inferMissingToolParams(toolName string, args map[string]any, missing []string, context string) map[string]any {
	inferred := cloneMap(args)
	if inferred == nil {
		inferred = map[string]any{}
	}

	tool := strings.ToLower(strings.TrimSpace(toolName))
	switch tool {
	case "task":
		if containsString(missing, "subagent_type") && valueMissing(inferred["subagent_type"]) {
			inferred["subagent_type"] = normalizeSubagentType(getStringAny(inferred["subagent_type"]))
			if valueMissing(inferred["subagent_type"]) {
				inferred["subagent_type"] = "general-purpose"
			}
		}
		if containsString(missing, "prompt") && valueMissing(inferred["prompt"]) {
			prompt := firstNonEmptyString(
				getStringAny(inferred["query"]),
				getStringAny(inferred["task"]),
				getStringAny(inferred["description"]),
				extractTaskFromContext(context),
			)
			if prompt != "" {
				inferred["prompt"] = prompt
			}
		}
		if containsString(missing, "description") && valueMissing(inferred["description"]) {
			description := getStringAny(inferred["prompt"])
			if description == "" {
				description = extractTaskFromContext(context)
			}
			description = strings.TrimSpace(description)
			if description == "" {
				description = "Execute task"
			}
			if len(description) > 50 {
				description = strings.TrimSpace(description[:50]) + "..."
			}
			inferred["description"] = description
		}

	case "bash":
		if containsString(missing, "command") && valueMissing(inferred["command"]) {
			cmd := firstNonEmptyString(
				getStringAny(inferred["cmd"]),
				getStringAny(inferred["shell"]),
				getStringAny(inferred["script"]),
			)
			if cmd != "" {
				inferred["command"] = cmd
			}
		}
		if containsString(missing, "description") && valueMissing(inferred["description"]) {
			cmd := strings.TrimSpace(getStringAny(inferred["command"]))
			if cmd != "" {
				first := strings.Fields(cmd)
				if len(first) > 0 {
					inferred["description"] = "Run " + first[0] + " command"
				}
			}
		}

	case "read":
		if containsString(missing, "file_path") && valueMissing(inferred["file_path"]) {
			path := firstNonEmptyString(
				getStringAny(inferred["path"]),
				getStringAny(inferred["file"]),
				getStringAny(inferred["filename"]),
			)
			if path != "" {
				inferred["file_path"] = path
			}
		}

	case "write":
		if containsString(missing, "file_path") && valueMissing(inferred["file_path"]) {
			path := firstNonEmptyString(
				getStringAny(inferred["path"]),
				getStringAny(inferred["file"]),
				getStringAny(inferred["filename"]),
			)
			if path != "" {
				inferred["file_path"] = path
			}
		}
		if containsString(missing, "content") && valueMissing(inferred["content"]) {
			content := firstNonEmptyString(
				getStringAny(inferred["text"]),
				getStringAny(inferred["data"]),
				getStringAny(inferred["body"]),
			)
			if content != "" {
				inferred["content"] = content
			}
		}

	case "grep":
		if containsString(missing, "pattern") && valueMissing(inferred["pattern"]) {
			pattern := firstNonEmptyString(
				getStringAny(inferred["query"]),
				getStringAny(inferred["search"]),
				getStringAny(inferred["regex"]),
			)
			if pattern != "" {
				inferred["pattern"] = pattern
			}
		}

	case "glob":
		if containsString(missing, "pattern") && valueMissing(inferred["pattern"]) {
			pattern := firstNonEmptyString(
				getStringAny(inferred["glob"]),
				getStringAny(inferred["path"]),
				getStringAny(inferred["search"]),
			)
			if pattern == "" {
				pattern = "**/*"
			}
			inferred["pattern"] = pattern
		}
	}

	return inferred
}

func extractTaskFromContext(context string) string {
	text := strings.TrimSpace(context)
	if text == "" {
		return ""
	}
	for _, pattern := range intentPatterns {
		if m := pattern.FindStringSubmatch(text); len(m) > 1 {
			return strings.TrimSpace(m[1])
		}
	}
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n'
	})
	for i := len(parts) - 1; i >= 0; i-- {
		s := strings.TrimSpace(parts[i])
		if len(s) >= 12 {
			return s
		}
	}
	if len(text) > 500 {
		return strings.TrimSpace(text[:500])
	}
	return text
}

func normalizeSubagentType(v string) string {
	val := strings.ToLower(strings.TrimSpace(v))
	switch {
	case val == "":
		return ""
	case strings.Contains(val, "explore"), strings.Contains(val, "codebase"), strings.Contains(val, "file"):
		return "Explore"
	case strings.Contains(val, "plan"), strings.Contains(val, "architect"):
		return "Plan"
	case strings.Contains(val, "analysis"),
		strings.Contains(val, "analyz"),
		strings.Contains(val, "config"),
		strings.Contains(val, "git"),
		strings.Contains(val, "test"),
		strings.Contains(val, "doc"),
		strings.Contains(val, "version"):
		return "general-purpose"
	default:
		return v
	}
}

func containsString(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func firstNonEmptyString(items ...string) string {
	for _, item := range items {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func getStringAny(v any) string {
	s, _ := v.(string)
	return s
}
