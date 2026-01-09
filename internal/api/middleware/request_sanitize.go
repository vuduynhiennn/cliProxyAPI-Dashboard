// Package middleware provides HTTP middleware components for the CLI Proxy API server.
// This file contains the request sanitization middleware that cleans up incompatible
// fields in OpenAI/Anthropic format requests before forwarding to Gemini API.
// Implements "Triple-Layer Sanitization" strategy for maximum compatibility.
package middleware

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	log "github.com/sirupsen/logrus"
)

var unsupportedRootFields = []string{
	"cache_control",
	"citations",
	"container",
	"metadata",
	"service_tier",
	"logprobs",
	"top_logprobs",
	"logit_bias",
	"parallel_tool_calls",
}

var unsupportedSchemaFields = []string{
	"additionalProperties",
	"$schema",
	"pattern",
	"exclusiveMinimum",
	"exclusiveMaximum",
	"minItems",
	"maxItems",
	"minLength",
	"maxLength",
	"default",
	"format",
	"examples",
	"$id",
	"$ref",
	"$defs",
	"definitions",
	"allOf",
	"anyOf",
	"oneOf",
	"not",
	"if",
	"then",
	"else",
	"dependentSchemas",
	"dependentRequired",
	"propertyNames",
	"unevaluatedProperties",
	"unevaluatedItems",
	"contentMediaType",
	"contentEncoding",
}

var unsupportedToolChoiceValues = map[string]bool{
	"validated": true,
	"required":  true,
}

func RequestSanitizeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != "POST" {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if !shouldSanitizeRequest(path) {
			c.Next()
			return
		}

		if c.Request.Body == nil {
			c.Next()
			return
		}

		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Next()
			return
		}

		sanitizedBody, stats := sanitizeRequestBody(bodyBytes)

		if stats.totalRemoved > 0 {
			log.Debugf("request_sanitized: path=%s removed=%d flattened=%d merged_system=%t",
				path, stats.totalRemoved, stats.flattenedMessages, stats.mergedSystem)
		}

		c.Request.Body = io.NopCloser(bytes.NewBuffer(sanitizedBody))
		c.Request.ContentLength = int64(len(sanitizedBody))

		c.Next()
	}
}

type sanitizeStats struct {
	totalRemoved      int
	flattenedMessages int
	mergedSystem      bool
}

func shouldSanitizeRequest(path string) bool {
	return strings.Contains(path, "/chat/completions") ||
		strings.Contains(path, "/completions") ||
		strings.Contains(path, "/responses") ||
		strings.Contains(path, "/messages")
}

func sanitizeRequestBody(body []byte) ([]byte, sanitizeStats) {
	stats := sanitizeStats{}

	if len(body) == 0 {
		return body, stats
	}

	if !gjson.ValidBytes(body) {
		return body, stats
	}

	result := body

	for _, field := range unsupportedRootFields {
		if gjson.GetBytes(result, field).Exists() {
			result, _ = sjson.DeleteBytes(result, field)
			stats.totalRemoved++
		}
	}

	result, tcRemoved := sanitizeToolChoice(result)
	stats.totalRemoved += tcRemoved

	result, cacheRemoved := removeCacheControlFromMessages(result)
	stats.totalRemoved += cacheRemoved

	result, toolRemoved := sanitizeToolSchemas(result)
	stats.totalRemoved += toolRemoved

	result, toolUseConverted := convertClaudeToolUseToOpenAI(result)
	stats.totalRemoved += toolUseConverted

	result, flattenCount := flattenMessageContent(result)
	stats.flattenedMessages = flattenCount

	result, emptyFixed := fixEmptyAssistantMessages(result)
	stats.totalRemoved += emptyFixed

	model := gjson.GetBytes(result, "model").String()
	if strings.Contains(strings.ToLower(model), "thinking") {
		var merged bool
		result, merged = mergeSystemToFirstUserMessage(result)
		stats.mergedSystem = merged
		if merged {
			stats.totalRemoved++
		}
	}

	result, systemRemoved := sanitizeSystemField(result)
	stats.totalRemoved += systemRemoved

	return result, stats
}

func sanitizeToolChoice(body []byte) ([]byte, int) {
	toolChoice := gjson.GetBytes(body, "tool_choice")
	if !toolChoice.Exists() {
		return body, 0
	}

	result := body
	removed := 0

	if toolChoice.Type == gjson.String {
		val := toolChoice.String()
		if unsupportedToolChoiceValues[val] {
			result, _ = sjson.SetBytes(result, "tool_choice", "auto")
			removed++
		}
	} else if toolChoice.IsObject() {
		tcType := toolChoice.Get("type").String()
		if tcType == "auto" || tcType == "" || tcType == "any" {
			result, _ = sjson.SetBytes(result, "tool_choice", "auto")
			removed++
		} else if tcType == "function" || tcType == "tool" {
			result, _ = sjson.DeleteBytes(result, "tool_choice")
			removed++
		}
	}

	return result, removed
}

func removeCacheControlFromMessages(body []byte) ([]byte, int) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, 0
	}

	removed := 0
	result := body

	for i, msg := range messages.Array() {
		if msg.Get("cache_control").Exists() {
			path := "messages." + itoa(i) + ".cache_control"
			result, _ = sjson.DeleteBytes(result, path)
			removed++
		}

		if msg.Get("name").Exists() {
			path := "messages." + itoa(i) + ".name"
			result, _ = sjson.DeleteBytes(result, path)
			removed++
		}

		content := msg.Get("content")
		if content.IsArray() {
			for j, item := range content.Array() {
				if item.Get("cache_control").Exists() {
					path := "messages." + itoa(i) + ".content." + itoa(j) + ".cache_control"
					result, _ = sjson.DeleteBytes(result, path)
					removed++
				}

				innerContent := item.Get("content")
				if innerContent.IsArray() {
					for k, innerItem := range innerContent.Array() {
						if innerItem.Get("cache_control").Exists() {
							path := "messages." + itoa(i) + ".content." + itoa(j) + ".content." + itoa(k) + ".cache_control"
							result, _ = sjson.DeleteBytes(result, path)
							removed++
						}
					}
				}
			}
		}
	}

	return result, removed
}


func convertClaudeToolUseToOpenAI(body []byte) ([]byte, int) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, 0
	}

	result := body
	converted := 0

	for i, msg := range messages.Array() {
		role := msg.Get("role").String()
		if role != "assistant" {
			continue
		}

		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}

		var textParts []string
		var toolCalls []map[string]interface{}

		for _, part := range content.Array() {
			partType := part.Get("type").String()

			if partType == "text" {
				text := part.Get("text").String()
				if text != "" {
					textParts = append(textParts, text)
				}
			} else if partType == "tool_use" {
				toolCall := map[string]interface{}{
					"id":   part.Get("id").String(),
					"type": "function",
					"function": map[string]interface{}{
						"name":      part.Get("name").String(),
						"arguments": part.Get("input").Raw,
					},
				}
				toolCalls = append(toolCalls, toolCall)
			}
		}

		if len(toolCalls) > 0 {
			msgPath := "messages." + itoa(i)

			contentStr := strings.Join(textParts, "\n")
			if contentStr == "" {
				contentStr = " "
			}
			result, _ = sjson.SetBytes(result, msgPath+".content", contentStr)

			for j, tc := range toolCalls {
				tcPath := msgPath + ".tool_calls." + itoa(j)
				result, _ = sjson.SetBytes(result, tcPath+".id", tc["id"])
				result, _ = sjson.SetBytes(result, tcPath+".type", tc["type"])
				fn := tc["function"].(map[string]interface{})
				result, _ = sjson.SetBytes(result, tcPath+".function.name", fn["name"])
				result, _ = sjson.SetRawBytes(result, tcPath+".function.arguments", []byte(fn["arguments"].(string)))
			}
			converted++
		}
	}

	for i, msg := range messages.Array() {
		role := msg.Get("role").String()
		if role != "user" {
			continue
		}

		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}

		for _, part := range content.Array() {
			if part.Get("type").String() == "tool_result" {
				toolUseId := part.Get("tool_use_id").String()
				resultContent := extractToolResultContent(part.Get("content"))
				if resultContent == "" {
					resultContent = "{}"
				}

				msgPath := "messages." + itoa(i)
				result, _ = sjson.SetBytes(result, msgPath+".role", "tool")
				result, _ = sjson.SetBytes(result, msgPath+".tool_call_id", toolUseId)
				result, _ = sjson.SetBytes(result, msgPath+".content", resultContent)
				converted++
				break
			}
		}
	}

	return result, converted
}

func extractToolResultContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}

	if content.IsArray() {
		var textParts []string
		for _, item := range content.Array() {
			if item.Get("type").String() == "text" {
				text := item.Get("text").String()
				if text != "" {
					textParts = append(textParts, text)
				}
			}
		}
		if len(textParts) > 0 {
			return strings.Join(textParts, "\n")
		}
	}

	if content.IsObject() {
		if text := content.Get("text"); text.Exists() {
			return text.String()
		}
	}

	return ""
}

func flattenMessageContent(body []byte) ([]byte, int) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, 0
	}

	result := body
	flattened := 0

	for i, msg := range messages.Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}

		var textParts []string
		for _, part := range content.Array() {
			if part.Get("type").String() == "text" {
				text := part.Get("text").String()
				if text != "" {
					textParts = append(textParts, text)
				}
			}
		}

		if len(textParts) > 0 {
			flattenedContent := strings.Join(textParts, "")
			path := "messages." + itoa(i) + ".content"
			result, _ = sjson.SetBytes(result, path, flattenedContent)
			flattened++
		}
	}

	return result, flattened
}

func fixEmptyAssistantMessages(body []byte) ([]byte, int) {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, 0
	}

	result := body
	fixed := 0
	indicesToRemove := []int{}

	for i, msg := range messages.Array() {
		role := msg.Get("role").String()
		if role != "assistant" {
			continue
		}

		content := msg.Get("content")
		hasToolCalls := msg.Get("tool_calls").Exists()

		contentEmpty := !content.Exists() || content.String() == "" || content.Raw == `""`

		if contentEmpty {
			if hasToolCalls {
				path := "messages." + itoa(i) + ".content"
				result, _ = sjson.SetBytes(result, path, " ")
				fixed++
			} else {
				indicesToRemove = append(indicesToRemove, i)
			}
		}
	}

	for j := len(indicesToRemove) - 1; j >= 0; j-- {
		idx := indicesToRemove[j]
		path := "messages." + itoa(idx)
		result, _ = sjson.DeleteBytes(result, path)
		fixed++
	}

	return result, fixed
}

func mergeSystemToFirstUserMessage(body []byte) ([]byte, bool) {
	system := gjson.GetBytes(body, "system")
	if !system.Exists() {
		return body, false
	}

	var systemText string
	if system.IsArray() {
		var parts []string
		for _, item := range system.Array() {
			if item.Get("type").String() == "text" {
				text := item.Get("text").String()
				if text != "" {
					parts = append(parts, text)
				}
			} else if item.Type == gjson.String {
				parts = append(parts, item.String())
			}
		}
		systemText = strings.Join(parts, "\n")
	} else if system.Type == gjson.String {
		systemText = system.String()
	}

	if systemText == "" {
		result, _ := sjson.DeleteBytes(body, "system")
		return result, true
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() || len(messages.Array()) == 0 {
		result, _ := sjson.DeleteBytes(body, "system")
		return result, true
	}

	result := body
	firstUserIdx := -1
	for i, msg := range messages.Array() {
		if msg.Get("role").String() == "user" {
			firstUserIdx = i
			break
		}
	}

	if firstUserIdx >= 0 {
		msg := messages.Array()[firstUserIdx]
		content := msg.Get("content")

		var newContent string
		if content.Type == gjson.String {
			newContent = "<system>\n" + systemText + "\n</system>\n\n" + content.String()
		} else if content.IsArray() {
			var textParts []string
			for _, part := range content.Array() {
				if part.Get("type").String() == "text" {
					textParts = append(textParts, part.Get("text").String())
				}
			}
			newContent = "<system>\n" + systemText + "\n</system>\n\n" + strings.Join(textParts, "")
		}

		if newContent != "" {
			path := "messages." + itoa(firstUserIdx) + ".content"
			result, _ = sjson.SetBytes(result, path, newContent)
		}
	}

	result, _ = sjson.DeleteBytes(result, "system")
	return result, true
}

func sanitizeToolSchemas(body []byte) ([]byte, int) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, 0
	}

	removed := 0
	result := body

	for i, tool := range tools.Array() {
		fn := tool.Get("function")
		if !fn.Exists() {
			inputSchema := tool.Get("input_schema")
			if inputSchema.Exists() {
				basePath := "tools." + itoa(i) + ".input_schema"
				var r int
				result, r = recursivelyRemoveSchemaFields(result, basePath, inputSchema)
				removed += r
			}
			continue
		}

		params := fn.Get("parameters")
		if params.Exists() {
			paramsPath := "tools." + itoa(i) + ".function.parameters"
			var r int
			result, r = recursivelyRemoveSchemaFields(result, paramsPath, params)
			removed += r
		}

		inputSchema := fn.Get("input_schema")
		if inputSchema.Exists() {
			inputPath := "tools." + itoa(i) + ".function.input_schema"
			var r int
			result, r = recursivelyRemoveSchemaFields(result, inputPath, inputSchema)
			removed += r
		}

		if fn.Get("strict").Exists() {
			result, _ = sjson.DeleteBytes(result, "tools."+itoa(i)+".function.strict")
			removed++
		}
	}

	return result, removed
}

func recursivelyRemoveSchemaFields(body []byte, basePath string, schema gjson.Result) ([]byte, int) {
	result := body
	removed := 0

	for _, field := range unsupportedSchemaFields {
		if schema.Get(field).Exists() {
			result, _ = sjson.DeleteBytes(result, basePath+"."+field)
			removed++
		}
	}

	props := schema.Get("properties")
	if props.IsObject() {
		props.ForEach(func(key, value gjson.Result) bool {
			propPath := basePath + ".properties." + key.String()
			var r int
			result, r = recursivelyRemoveSchemaFields(result, propPath, value)
			removed += r
			return true
		})
	}

	items := schema.Get("items")
	if items.Exists() && items.IsObject() {
		itemsPath := basePath + ".items"
		var r int
		result, r = recursivelyRemoveSchemaFields(result, itemsPath, items)
		removed += r
	}

	return result, removed
}

func sanitizeSystemField(body []byte) ([]byte, int) {
	system := gjson.GetBytes(body, "system")
	if !system.Exists() {
		return body, 0
	}

	removed := 0
	result := body

	if system.IsArray() {
		for i, item := range system.Array() {
			if item.Get("cache_control").Exists() {
				path := "system." + itoa(i) + ".cache_control"
				result, _ = sjson.DeleteBytes(result, path)
				removed++
			}
		}
	}

	return result, removed
}

func itoa(i int) string {
	if i < 10 && i >= 0 {
		return string(rune('0' + i))
	}
	return fmt.Sprintf("%d", i)
}
