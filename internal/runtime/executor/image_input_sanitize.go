package executor

import (
	"encoding/json"
	"strings"
)

func opencodeGoSanitizeHistoricalImages(payload []byte) ([]byte, bool) {
	return sanitizeImageInputs(payload, "[Image omitted from previous turn.]", false)
}

func sanitizeImageInputs(payload []byte, placeholder string, alwaysPlaceholder bool) ([]byte, bool) {
	if len(payload) == 0 || !json.Valid(payload) {
		return payload, false
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload, false
	}
	changed := false
	if messages, ok := root["messages"].([]any); ok {
		if sanitizeImageMessageArray(messages, placeholder, alwaysPlaceholder) {
			root["messages"] = messages
			changed = true
		}
	}
	if input, ok := root["input"]; ok {
		if sanitized, ok := sanitizeImageResponsesInput(input, placeholder, alwaysPlaceholder); ok {
			root["input"] = sanitized
			changed = true
		}
	}
	if !changed {
		return payload, false
	}
	out, err := json.Marshal(root)
	if err != nil {
		return payload, false
	}
	return out, true
}

func sanitizeImageMessageArray(messages []any, placeholder string, alwaysPlaceholder bool) bool {
	changed := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"]
		if !ok {
			continue
		}
		if sanitized, ok := sanitizeImageContentParts(content, "text", placeholder, alwaysPlaceholder); ok {
			message["content"] = sanitized
			changed = true
		}
	}
	return changed
}

func sanitizeImageResponsesInput(input any, placeholder string, alwaysPlaceholder bool) (any, bool) {
	switch typed := input.(type) {
	case []any:
		changed := false
		for i, raw := range typed {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if opencodeGoContentPartIsImage(item) {
				typed[i] = imagePlaceholderPart("input_text", placeholder)
				changed = true
				continue
			}
			content, ok := item["content"]
			if !ok {
				continue
			}
			if sanitized, ok := sanitizeImageContentParts(content, "input_text", placeholder, alwaysPlaceholder); ok {
				item["content"] = sanitized
				changed = true
			}
		}
		return typed, changed
	case map[string]any:
		if opencodeGoContentPartIsImage(typed) {
			return imagePlaceholderPart("input_text", placeholder), true
		}
		content, ok := typed["content"]
		if !ok {
			return input, false
		}
		if sanitized, ok := sanitizeImageContentParts(content, "input_text", placeholder, alwaysPlaceholder); ok {
			typed["content"] = sanitized
			return typed, true
		}
	}
	return input, false
}

func sanitizeImageContentParts(content any, placeholderType, placeholder string, alwaysPlaceholder bool) (any, bool) {
	parts, ok := content.([]any)
	if !ok {
		return content, false
	}
	changed := false
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok || !opencodeGoContentPartIsImage(partMap) {
			out = append(out, part)
			continue
		}
		if alwaysPlaceholder || !opencodeGoContentPartsHaveText(out) {
			out = append(out, imagePlaceholderPart(placeholderType, placeholder))
		}
		changed = true
	}
	if !changed {
		return content, false
	}
	if len(out) == 0 {
		out = append(out, imagePlaceholderPart(placeholderType, placeholder))
	}
	return out, true
}

func opencodeGoContentPartIsImage(part map[string]any) bool {
	rawType, _ := part["type"].(string)
	switch strings.ToLower(strings.TrimSpace(rawType)) {
	case "image", "image_url", "input_image":
		return true
	}
	_, ok := part["image_url"]
	return ok
}

func opencodeGoContentPartsHaveText(parts []any) bool {
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			continue
		}
		rawType, _ := partMap["type"].(string)
		switch strings.ToLower(strings.TrimSpace(rawType)) {
		case "text", "input_text", "output_text":
			if text, _ := partMap["text"].(string); strings.TrimSpace(text) != "" {
				return true
			}
		}
	}
	return false
}

func imagePlaceholderPart(partType, placeholder string) map[string]any {
	partType = strings.TrimSpace(partType)
	if partType == "" {
		partType = "text"
	}
	placeholder = strings.TrimSpace(placeholder)
	if placeholder == "" {
		placeholder = "[Image omitted.]"
	}
	return map[string]any{
		"type": partType,
		"text": placeholder,
	}
}
