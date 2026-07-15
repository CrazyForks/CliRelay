package executor

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteMetadata = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"

	// Sub2API-style guidance: hosted image_generation is intentional for clients that
	// cannot expose the local image_gen namespace (API-key custom providers).
	codexImageGenerationBridgeMarker = "<cliproxy-codex-image-generation>"
	codexImageGenerationBridgeText   = codexImageGenerationBridgeMarker + "\nWhen the user asks for raster image generation or editing, use the OpenAI Responses native `image_generation` tool attached to this request. The local Codex client may not expose an `image_gen` namespace under custom/API-key providers; that does not mean image generation is unavailable. Do not claim the environment lacks image tooling solely because `image_gen` is absent, and do not ask the user to switch to CLI fallback as the primary fix.\n</cliproxy-codex-image-generation>"
)

var (
	imageGenToolJSON      = []byte(`{"type":"image_generation","output_format":"png"}`)
	imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)
)

// maybeEnsureCodexImageGenerationTool prepares outbound /responses tools for image gen.
//
// Policy (root-cause fix for Desktop):
//  1. If the client already advertises local image_gen (namespace/function), keep it and
//     strip hosted image_generation so Desktop uses /v1/images + disk save path.
//  2. Else if account bridge is enabled, inject hosted image_generation + bridge instructions.
//     API-key custom providers typically do not expose local image_gen, so hosted is required.
//  3. When image intent is present and only hosted tool is available, force
//     tool_choice=image_generation so the model cannot skip the tool and reply with text.
func maybeEnsureCodexImageGenerationTool(body []byte, auth *cliproxyauth.Auth, baseModel string, headers http.Header) []byte {
	if requestHasLocalImageGenTool(body) {
		return stripHostedImageGenerationTools(body)
	}
	if !codexImageGenerationBridgeEnabled(auth) {
		return body
	}
	body = ensureCodexImageGenerationTool(body, baseModel, auth, headers)
	if requestHasHostedImageGenerationTool(body) {
		body = ensureCodexImageGenerationBridgeInstructions(body)
		if requestLooksLikeImageGenerationIntent(body) {
			body = forceCodexImageGenerationToolChoice(body)
		}
	}
	return body
}

func requestLooksLikeImageGenerationIntent(body []byte) bool {
	// Explicit Image Gen skill / slash-command markers from Codex Desktop.
	markers := []string{
		"[$imagegen]",
		"<name>imagegen</name>",
		"skills/.system/imagegen",
		"$imagegen",
		"image_gen.imagegen",
		"built-in `image_gen`",
		"built-in image_gen",
	}
	// Prefer scanning recent user text, not the whole body (tools/schema can be huge).
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		lower := strings.ToLower(input.String())
		for _, m := range markers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return true
			}
		}
		return looksLikeChineseImageRequest(lower) || looksLikeEnglishImageRequest(lower)
	}
	if input.IsArray() {
		// Scan from the end: last user turn is decisive.
		items := input.Array()
		for i := len(items) - 1; i >= 0 && i >= len(items)-8; i-- {
			item := items[i]
			role := strings.TrimSpace(item.Get("role").String())
			if role != "" && role != "user" {
				continue
			}
			text := extractCodexInputItemText(item)
			if text == "" {
				continue
			}
			lower := strings.ToLower(text)
			for _, m := range markers {
				if strings.Contains(lower, strings.ToLower(m)) {
					return true
				}
			}
			if looksLikeChineseImageRequest(lower) || looksLikeEnglishImageRequest(lower) {
				return true
			}
		}
	}
	return false
}

func extractCodexInputItemText(item gjson.Result) string {
	if item.Get("content").Type == gjson.String {
		return item.Get("content").String()
	}
	content := item.Get("content")
	if !content.IsArray() {
		if t := item.Get("text"); t.Type == gjson.String {
			return t.String()
		}
		return ""
	}
	var b strings.Builder
	for _, part := range content.Array() {
		switch strings.TrimSpace(part.Get("type").String()) {
		case "input_text", "output_text", "text":
			if t := part.Get("text"); t.Type == gjson.String {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(t.String())
			}
		}
	}
	return b.String()
}

func looksLikeChineseImageRequest(lower string) bool {
	// lower is already lowercased for ASCII; Chinese unchanged.
	keys := []string{"画一", "画个", "画只", "画一张", "生成一张", "生成图片", "生图", "出图", "改图", "修图", "绘一张"}
	for _, k := range keys {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func looksLikeEnglishImageRequest(lower string) bool {
	keys := []string{
		"generate an image", "generate a image", "draw a ", "draw an ", "create an image",
		"make an image", "edit this image", "image generation", "generate image",
	}
	for _, k := range keys {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func forceCodexImageGenerationToolChoice(body []byte) []byte {
	body, _ = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"image_generation"}`))
	return body
}

func requestHasLocalImageGenTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if isImageGenerationFunctionTool(tool) {
				return true
			}
		}
	}
	// Responses Lite embeds tools inside input additional_tools items.
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
			continue
		}
		nested := item.Get("tools")
		if !nested.IsArray() {
			continue
		}
		for _, tool := range nested.Array() {
			if isImageGenerationFunctionTool(tool) {
				return true
			}
		}
	}
	return false
}

func requestHasHostedImageGenerationTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
			return true
		}
	}
	return false
}

// stripHostedImageGenerationTools removes Responses-native image_generation tools
// so the model prefers the client's local image_gen namespace when present.
func stripHostedImageGenerationTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		kept := make([]any, 0, len(tools.Array()))
		removed := false
		for _, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
				removed = true
				continue
			}
			kept = append(kept, tool.Value())
		}
		if removed {
			if len(kept) == 0 {
				body, _ = sjson.DeleteBytes(body, "tools")
			} else {
				body, _ = sjson.SetBytes(body, "tools", kept)
			}
		}
	}
	choiceType := strings.TrimSpace(gjson.GetBytes(body, "tool_choice.type").String())
	if choiceType == "image_generation" {
		body, _ = sjson.SetBytes(body, "tool_choice", "auto")
	}
	return body
}

func ensureCodexImageGenerationBridgeInstructions(body []byte) []byte {
	instructions := gjson.GetBytes(body, "instructions")
	if instructions.Exists() && instructions.Type == gjson.String {
		text := instructions.String()
		if strings.Contains(text, codexImageGenerationBridgeMarker) {
			return body
		}
		if strings.TrimSpace(text) == "" {
			body, _ = sjson.SetBytes(body, "instructions", codexImageGenerationBridgeText)
			return body
		}
		body, _ = sjson.SetBytes(body, "instructions", text+"\n\n"+codexImageGenerationBridgeText)
		return body
	}
	body, _ = sjson.SetBytes(body, "instructions", codexImageGenerationBridgeText)
	return body
}

func codexImageGenerationBridgeEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	enabled, _ := auth.Metadata["codex_image_generation_bridge"].(bool)
	return enabled
}

func ensureCodexImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header) []byte {
	if isCodexResponsesLiteRequest(body, headers) {
		return body
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(baseModel)), "spark") {
		return body
	}
	if isCodexFreePlanAuth(auth) {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", imageGenToolArrayJSON)
		return body
	}
	for _, tool := range tools.Array() {
		if tool.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(tool) {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tools.-1", imageGenToolJSON)
	return body
}

func isImageGenerationFunctionTool(tool gjson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		name := tool.Get("name").String()
		return name == "image_gen.imagegen" || name == "imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		nested := tool.Get("tools")
		if !nested.IsArray() {
			return false
		}
		for _, nestedTool := range nested.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

func isCodexResponsesLiteRequest(body []byte, headers http.Header) bool {
	if headers != nil && strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true") {
		return true
	}
	value := gjson.GetBytes(body, codexResponsesLiteMetadata)
	if !value.Exists() {
		return false
	}
	return value.Type == gjson.True || (value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.String()), "true"))
}

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

// normalizeCodexImageGenerationOutboundEvent normalizes hosted image events for clients:
//  1. force status=completed when result is present
//  2. synthesize an assistant markdown image message so Desktop can render without saved_path
func normalizeCodexImageGenerationOutboundEvent(payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	normalized := normalizeCodexImageGenerationCallStatus(payload)
	out := [][]byte{normalized}
	if msg := synthesizeCodexImageDisplayMessageEvent(normalized); len(msg) > 0 {
		out = append(out, msg)
	}
	return out
}

// normalizeCodexImageGenerationCallStatus upgrades image_generation_call items that already
// carry a finished image payload but remain status=generating.
func normalizeCodexImageGenerationCallStatus(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	hadSSEPrefix := bytes.HasPrefix(payload, dataTag)
	body := payload
	if hadSSEPrefix {
		body = bytes.TrimSpace(payload[len(dataTag):])
	}
	if !gjson.ValidBytes(body) {
		return payload
	}

	eventType := gjson.GetBytes(body, "type").String()
	switch eventType {
	case "response.output_item.done", "response.output_item.added":
		if !shouldCompleteImageGenerationCall(gjson.GetBytes(body, "item")) {
			return payload
		}
		updated, err := sjson.SetBytes(body, "item.status", "completed")
		if err != nil {
			return payload
		}
		return maybeWrapSSEData(hadSSEPrefix, updated)
	case "response.completed", "response.done":
		output := gjson.GetBytes(body, "response.output")
		if !output.IsArray() {
			return payload
		}
		updated := body
		changed := false
		for index, item := range output.Array() {
			if !shouldCompleteImageGenerationCall(item) {
				continue
			}
			path := "response.output." + strconv.Itoa(index) + ".status"
			next, err := sjson.SetBytes(updated, path, "completed")
			if err != nil {
				return payload
			}
			updated = next
			changed = true
		}
		if !changed {
			return payload
		}
		return maybeWrapSSEData(hadSSEPrefix, updated)
	default:
		return payload
	}
}

// synthesizeCodexImageDisplayMessageEvent turns a completed hosted image_generation_call
// into an assistant markdown image message. Desktop custom/API-key providers often cannot
// expose local image_gen, so hosted base64 results otherwise stay invisible.
func synthesizeCodexImageDisplayMessageEvent(payload []byte) []byte {
	hadSSEPrefix := bytes.HasPrefix(payload, dataTag)
	body := payload
	if hadSSEPrefix {
		body = bytes.TrimSpace(payload[len(dataTag):])
	}
	if !gjson.ValidBytes(body) {
		return nil
	}
	if gjson.GetBytes(body, "type").String() != "response.output_item.done" {
		return nil
	}
	item := gjson.GetBytes(body, "item")
	if strings.TrimSpace(item.Get("type").String()) != "image_generation_call" {
		return nil
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return nil
	}
	status := strings.ToLower(strings.TrimSpace(item.Get("status").String()))
	if status != "" && status != "completed" && status != "generating" && status != "in_progress" && status != "incomplete" {
		return nil
	}
	format := strings.ToLower(strings.TrimSpace(item.Get("output_format").String()))
	mime := "image/png"
	switch format {
	case "jpeg", "jpg":
		mime = "image/jpeg"
	case "webp":
		mime = "image/webp"
	case "gif":
		mime = "image/gif"
	}
	// Markdown image so Desktop/agent markdown renderers can show the asset inline.
	text := fmt.Sprintf("![generated image](data:%s;base64,%s)", mime, result)
	msgID := strings.TrimSpace(item.Get("id").String())
	if msgID == "" {
		msgID = "msg_image_display"
	} else {
		msgID = "msg_" + msgID + "_display"
	}
	event := []byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":""}]}}`)
	event, _ = sjson.SetBytes(event, "item.id", msgID)
	event, _ = sjson.SetBytes(event, "item.content.0.text", text)
	if revised := strings.TrimSpace(item.Get("revised_prompt").String()); revised != "" {
		// Keep caption short; full prompt can be huge.
		if len(revised) > 200 {
			revised = revised[:200] + "…"
		}
		caption := "Generated image: " + revised + "\n\n" + text
		event, _ = sjson.SetBytes(event, "item.content.0.text", caption)
	}
	return maybeWrapSSEData(hadSSEPrefix, event)
}

func shouldCompleteImageGenerationCall(item gjson.Result) bool {
	if !item.Exists() || !item.IsObject() {
		return false
	}
	if strings.TrimSpace(item.Get("type").String()) != "image_generation_call" {
		return false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("status").String())) {
	case "", "generating", "in_progress", "incomplete":
		return true
	default:
		return false
	}
}

func maybeWrapSSEData(hadSSEPrefix bool, body []byte) []byte {
	if !hadSSEPrefix {
		return body
	}
	out := make([]byte, 0, len(dataTag)+1+len(body))
	out = append(out, dataTag...)
	out = append(out, ' ')
	out = append(out, body...)
	return out
}
