package executor

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexResponsesImageSizeHintPrefix = "Preferred image size: "

func ensureTranslatedCodexModel(body []byte, fallback string) []byte {
	if strings.TrimSpace(gjson.GetBytes(body, "model").String()) != "" {
		return body
	}
	return util.MutateTopLevelObject(body, map[string][]byte{
		"model": util.JSONString(fallback),
	}, nil)
}

func sanitizeCodexResponsesRequest(body []byte) []byte {
	body = util.MutateTopLevelObject(body, nil, []string{
		"max_output_tokens",
		"max_completion_tokens",
		"max_tokens",
	})
	body = stripCodexResponsesImageGenerationSize(body)
	// History hygiene: never forward multi-MB data:image blobs from Desktop session replay.
	body = stripCodexHistoryDataURLImages(body)
	return body
}

func stripCodexResponsesImageGenerationSize(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	var sizeHint string
	for index, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) != "image_generation" {
			continue
		}
		sizePath := fmt.Sprintf("tools.%d.size", index)
		size := gjson.GetBytes(body, sizePath)
		if !size.Exists() {
			continue
		}
		if trimmed := strings.TrimSpace(size.String()); trimmed != "" && sizeHint == "" {
			sizeHint = trimmed
		}
		body, _ = sjson.DeleteBytes(body, sizePath)
	}
	return appendCodexResponsesImageSizeHint(body, sizeHint)
}

func appendCodexResponsesImageSizeHint(body []byte, size string) []byte {
	size = strings.TrimSpace(size)
	if size == "" {
		return body
	}
	hint := codexResponsesImageSizeHintPrefix + size + "."
	inputResult := gjson.GetBytes(body, "input")
	if inputResult.Type == gjson.String {
		body, _ = sjson.SetBytes(body, "input", appendCodexResponsesImageHintToText(inputResult.String(), hint))
		return body
	}
	if inputResult.IsArray() {
		items := inputResult.Array()
		for itemIndex, item := range items {
			if strings.TrimSpace(item.Get("role").String()) != "user" {
				continue
			}
			if updated, ok := appendCodexResponsesImageHintToInputItem(body, itemIndex, item, hint); ok {
				return updated
			}
		}
		for itemIndex, item := range items {
			if updated, ok := appendCodexResponsesImageHintToInputItem(body, itemIndex, item, hint); ok {
				return updated
			}
		}
		body, _ = sjson.SetRawBytes(body, "input.-1", codexResponsesUserTextMessage(hint))
		return body
	}
	body, _ = sjson.SetRawBytes(body, "input", []byte(`[]`))
	body, _ = sjson.SetRawBytes(body, "input.-1", codexResponsesUserTextMessage(hint))
	return body
}

func appendCodexResponsesImageHintToInputItem(body []byte, itemIndex int, item gjson.Result, hint string) ([]byte, bool) {
	content := item.Get("content")
	if content.Type == gjson.String {
		path := fmt.Sprintf("input.%d.content", itemIndex)
		body, _ = sjson.SetBytes(body, path, appendCodexResponsesImageHintToText(content.String(), hint))
		return body, true
	}
	if !content.IsArray() {
		return body, false
	}
	for contentIndex, part := range content.Array() {
		text := part.Get("text")
		if text.Type != gjson.String {
			continue
		}
		path := fmt.Sprintf("input.%d.content.%d.text", itemIndex, contentIndex)
		body, _ = sjson.SetBytes(body, path, appendCodexResponsesImageHintToText(text.String(), hint))
		return body, true
	}
	path := fmt.Sprintf("input.%d.content.-1", itemIndex)
	body, _ = sjson.SetRawBytes(body, path, codexResponsesInputTextMessagePart(hint))
	return body, true
}

func appendCodexResponsesImageHintToText(text string, hint string) string {
	if strings.Contains(text, hint) || strings.Contains(text, codexResponsesImageSizeHintPrefix) {
		return text
	}
	text = strings.TrimRight(text, " \t\r\n")
	if text == "" {
		return hint
	}
	return text + "\n\n" + hint
}

func codexResponsesUserTextMessage(text string) []byte {
	message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
	message, _ = sjson.SetBytes(message, "content.0.text", text)
	return message
}

func codexResponsesInputTextMessagePart(text string) []byte {
	part := []byte(`{"type":"input_text","text":""}`)
	part, _ = sjson.SetBytes(part, "text", text)
	return part
}

func extractCodexResponsesOutputItemDone(payload []byte) ([]byte, string, bool) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return nil, "", false
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || item.Raw == "" {
		return nil, "", false
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = item.Raw
	}
	return []byte(item.Raw), key, true
}

func mergeCodexResponsesCompletedOutput(payload []byte, pendingItems [][]byte, pendingKeys []string) []byte {
	if len(pendingItems) == 0 || gjson.GetBytes(payload, "type").String() != "response.completed" {
		return payload
	}

	output := gjson.GetBytes(payload, "response.output")
	merged := make([][]byte, 0, len(pendingItems)+len(output.Array()))
	seen := make(map[string]struct{}, len(pendingItems)+len(output.Array()))

	if output.IsArray() {
		for _, item := range output.Array() {
			raw := []byte(item.Raw)
			key := strings.TrimSpace(item.Get("id").String())
			if key == "" {
				key = item.Raw
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, raw)
		}
	}

	appended := false
	for idx, item := range pendingItems {
		key := pendingKeys[idx]
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, item)
		appended = true
	}

	if !appended {
		return payload
	}

	buf := bytes.NewBuffer(make([]byte, 0, len(payload)+len(pendingItems)*64))
	buf.WriteByte('[')
	for idx, item := range merged {
		if idx > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item)
	}
	buf.WriteByte(']')

	updated, err := sjson.SetRawBytes(payload, "response.output", buf.Bytes())
	if err != nil {
		return payload
	}
	return updated
}
