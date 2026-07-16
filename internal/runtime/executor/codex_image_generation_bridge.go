package executor

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
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

	// Codex Desktop / ChatGPT-style sandbox image refs written into assistant markdown.
	// Only rewrite these when the same response already produced a hosted image_generation_call.
	codexMntDataMarkdownRef = regexp.MustCompile(`!\[([^\]]*)\]\((?:sandbox:)?(/mnt/data/(\d+)\.([A-Za-z0-9]+))\)`)
	codexMntDataBareRef     = regexp.MustCompile(`(?:sandbox:)?/mnt/data/(\d+)\.([A-Za-z0-9]+)`)

	// Inbound history hygiene: Desktop re-sends prior assistant text that may contain multi-MB
	// data:image URLs from our outbound display rewrite. Strip pixels, keep a short placeholder
	// so the model still knows an image existed — zero server-side image storage.
	codexMarkdownDataURLImage = regexp.MustCompile(`!\[([^\]]*)\]\((data:image\/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=\r\n]{256,})\)`)
	codexBareDataURLImage     = regexp.MustCompile(`data:image\/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=\r\n]{256,}`)

	// Any markdown image whose target is not a browser-renderable URL (data:/http:/https:).
	// Models often invent ~/.codex/generated_images/... paths under custom providers.
	codexMarkdownAnyImageRef = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
)

// codexHistoryImage is a single image extracted from the current request body only.
// Never written to disk or process-global cache — lives only for this sanitize call.
type codexHistoryImage struct {
	MIME   string
	Result string
}

// codexHostedImage is one completed hosted image_generation_call payload for Desktop rewrite.
type codexHostedImage struct {
	Result string
	MIME   string
}

// codexImageStreamNormalizer rewrites Desktop-visible assistant markdown after hosted image gen.
//
// Codex Desktop keeps the model text message (often `![x](/mnt/data/0.png)`) and drops the
// synthetic data-url message we inject beside image_generation_call. When this response
// already has image result(s), replace sandbox /mnt/data refs with data URLs in the
// assistant text events Desktop actually persists.
type codexImageStreamNormalizer struct {
	images []codexHostedImage
}

func newCodexImageStreamNormalizer() *codexImageStreamNormalizer {
	return &codexImageStreamNormalizer{}
}

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
//  2. cache image results and rewrite Desktop assistant markdown /mnt/data refs to data URLs
//  3. synthesize a fallback data-url message (Desktop may drop it; rewrite is the primary path)
//
// n may be nil; then only status + synthetic display message run (stateless).
func normalizeCodexImageGenerationOutboundEvent(payload []byte) [][]byte {
	return normalizeCodexImageGenerationOutboundEventWithState(nil, payload)
}

func normalizeCodexImageGenerationOutboundEventWithState(n *codexImageStreamNormalizer, payload []byte) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	normalized := normalizeCodexImageGenerationCallStatus(payload)
	if n != nil {
		n.observe(normalized)
		normalized = n.rewriteAssistantMarkdown(normalized)
	}
	out := [][]byte{normalized}
	if msg := synthesizeCodexImageDisplayMessageEvent(normalized); len(msg) > 0 {
		out = append(out, msg)
	}
	return out
}

func (n *codexImageStreamNormalizer) observe(payload []byte) {
	if n == nil || len(payload) == 0 {
		return
	}
	body := sseJSONBody(payload)
	if !gjson.ValidBytes(body) {
		return
	}
	switch gjson.GetBytes(body, "type").String() {
	case "response.output_item.done", "response.output_item.added":
		n.rememberImageItem(gjson.GetBytes(body, "item"))
	case "response.completed", "response.done":
		output := gjson.GetBytes(body, "response.output")
		if !output.IsArray() {
			return
		}
		for _, item := range output.Array() {
			n.rememberImageItem(item)
		}
	}
}

func (n *codexImageStreamNormalizer) rememberImageItem(item gjson.Result) {
	if !item.Exists() || !item.IsObject() {
		return
	}
	if strings.TrimSpace(item.Get("type").String()) != "image_generation_call" {
		return
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return
	}
	// Avoid duplicate cache if both added/done carry the same result.
	for _, existing := range n.images {
		if existing.Result == result {
			return
		}
	}
	n.images = append(n.images, codexHostedImage{
		Result: result,
		MIME:   codexImageMIMEFromFormat(item.Get("output_format").String()),
	})
}

func (n *codexImageStreamNormalizer) rewriteAssistantMarkdown(payload []byte) []byte {
	if n == nil || len(n.images) == 0 || len(payload) == 0 {
		return payload
	}
	hadSSEPrefix := bytes.HasPrefix(payload, dataTag)
	body := sseJSONBody(payload)
	if !gjson.ValidBytes(body) {
		return payload
	}

	eventType := gjson.GetBytes(body, "type").String()
	updated := body
	changed := false

	switch eventType {
	case "response.output_text.done":
		text := gjson.GetBytes(body, "text").String()
		if next, ok := ensureCodexAssistantImageMarkdown(text, n.images); ok {
			var err error
			updated, err = sjson.SetBytes(updated, "text", next)
			if err != nil {
				return payload
			}
			changed = true
		}
	case "response.content_part.done":
		partType := strings.TrimSpace(gjson.GetBytes(body, "part.type").String())
		if partType == "output_text" || partType == "text" {
			text := gjson.GetBytes(body, "part.text").String()
			if next, ok := ensureCodexAssistantImageMarkdown(text, n.images); ok {
				var err error
				updated, err = sjson.SetBytes(updated, "part.text", next)
				if err != nil {
					return payload
				}
				changed = true
			}
		}
	case "response.output_item.done", "response.output_item.added":
		item := gjson.GetBytes(body, "item")
		if strings.TrimSpace(item.Get("type").String()) != "message" {
			return payload
		}
		// Skip our synthetic display item; Desktop ignores it and we must not recurse on it.
		if isCodexSyntheticImageDisplayMessageID(item.Get("id").String()) {
			return payload
		}
		// Only rewrite completed assistant messages Desktop persists as agent text.
		role := strings.TrimSpace(item.Get("role").String())
		if role != "" && role != "assistant" {
			return payload
		}
		// output_item.added is often empty; only rewrite/append on done so deltas stay coherent.
		appendOK := eventType == "response.output_item.done"
		content := item.Get("content")
		if !content.IsArray() {
			return payload
		}
		for i, part := range content.Array() {
			partType := strings.TrimSpace(part.Get("type").String())
			if partType != "output_text" && partType != "text" {
				continue
			}
			text := part.Get("text").String()
			var next string
			var ok bool
			if appendOK {
				next, ok = ensureCodexAssistantImageMarkdown(text, n.images)
			} else {
				next, ok = rewriteCodexMntDataImageMarkdown(text, n.images)
			}
			if !ok {
				continue
			}
			path := "item.content." + strconv.Itoa(i) + ".text"
			var err error
			updated, err = sjson.SetBytes(updated, path, next)
			if err != nil {
				return payload
			}
			changed = true
		}
	case "response.completed", "response.done":
		output := gjson.GetBytes(body, "response.output")
		if !output.IsArray() {
			return payload
		}
		for i, item := range output.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "message" {
				continue
			}
			if isCodexSyntheticImageDisplayMessageID(item.Get("id").String()) {
				continue
			}
			role := strings.TrimSpace(item.Get("role").String())
			if role != "" && role != "assistant" {
				continue
			}
			content := item.Get("content")
			if !content.IsArray() {
				continue
			}
			for j, part := range content.Array() {
				partType := strings.TrimSpace(part.Get("type").String())
				if partType != "output_text" && partType != "text" {
					continue
				}
				text := part.Get("text").String()
				next, ok := ensureCodexAssistantImageMarkdown(text, n.images)
				if !ok {
					continue
				}
				path := "response.output." + strconv.Itoa(i) + ".content." + strconv.Itoa(j) + ".text"
				var err error
				updated, err = sjson.SetBytes(updated, path, next)
				if err != nil {
					return payload
				}
				changed = true
			}
		}
	default:
		return payload
	}
	if !changed {
		return payload
	}
	return maybeWrapSSEData(hadSSEPrefix, updated)
}

// ensureCodexAssistantImageMarkdown makes Desktop-visible assistant text show hosted images.
//  1. Rewrite ChatGPT sandbox refs: ![x](/mnt/data/0.png) -> data URL
//  2. Rewrite non-renderable local paths: ![x](/Users/.../generated_images/a.png) -> data URL
//  3. Only if still no data:image, append one markdown data URL (never stack on top of a fake path).
func ensureCodexAssistantImageMarkdown(text string, images []codexHostedImage) (string, bool) {
	if len(images) == 0 {
		return text, false
	}
	changed := false
	if next, ok := rewriteCodexMntDataImageMarkdown(text, images); ok {
		text = next
		changed = true
	}
	if next, ok := rewriteCodexNonRenderableImageMarkdown(text, images); ok {
		text = next
		changed = true
	}
	// Already has a renderable data image — do not append a second copy (double-thumbnail bug).
	if strings.Contains(text, "data:image/") {
		return text, changed
	}
	return appendCodexHostedImageMarkdown(text, images), true
}

func rewriteCodexMntDataImageMarkdown(text string, images []codexHostedImage) (string, bool) {
	if text == "" || len(images) == 0 || !strings.Contains(text, "/mnt/data/") {
		return text, false
	}
	// Prefer markdown image refs so alt text is preserved.
	out := codexMntDataMarkdownRef.ReplaceAllStringFunc(text, func(match string) string {
		sub := codexMntDataMarkdownRef.FindStringSubmatch(match)
		if len(sub) != 5 {
			return match
		}
		img, ok := codexHostedImageByIndex(images, sub[3])
		if !ok {
			return match
		}
		return fmt.Sprintf("![%s](data:%s;base64,%s)", sub[1], img.MIME, img.Result)
	})
	// Bare /mnt/data/N.ext left outside markdown (rare).
	if strings.Contains(out, "/mnt/data/") {
		out = codexMntDataBareRef.ReplaceAllStringFunc(out, func(match string) string {
			sub := codexMntDataBareRef.FindStringSubmatch(match)
			if len(sub) != 3 {
				return match
			}
			img, ok := codexHostedImageByIndex(images, sub[1])
			if !ok {
				return match
			}
			return fmt.Sprintf("data:%s;base64,%s", img.MIME, img.Result)
		})
	}
	if out == text {
		return text, false
	}
	return out, true
}

func appendCodexHostedImageMarkdown(text string, images []codexHostedImage) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(text, " \t\r\n"))
	for i, img := range images {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		alt := "generated image"
		if len(images) > 1 {
			alt = fmt.Sprintf("generated image %d", i+1)
		}
		b.WriteString("![")
		b.WriteString(alt)
		b.WriteString("](data:")
		b.WriteString(img.MIME)
		b.WriteString(";base64,")
		b.WriteString(img.Result)
		b.WriteByte(')')
	}
	return b.String()
}

// rewriteCodexNonRenderableImageMarkdown replaces ![alt](local-or-fake-path) with data URLs.
// Leaves data:/http(s):/cliproxy-image: targets alone.
func rewriteCodexNonRenderableImageMarkdown(text string, images []codexHostedImage) (string, bool) {
	if text == "" || len(images) == 0 || !strings.Contains(text, "![") {
		return text, false
	}
	seq := 0
	out := codexMarkdownAnyImageRef.ReplaceAllStringFunc(text, func(match string) string {
		sub := codexMarkdownAnyImageRef.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		alt, target := sub[1], strings.TrimSpace(sub[2])
		if target == "" || isCodexRenderableImageTarget(target) {
			return match
		}
		img := images[0]
		if seq < len(images) {
			img = images[seq]
		} else {
			img = images[len(images)-1]
		}
		seq++
		return fmt.Sprintf("![%s](data:%s;base64,%s)", alt, img.MIME, img.Result)
	})
	if out == text {
		return text, false
	}
	return out, true
}

func isCodexRenderableImageTarget(target string) bool {
	lower := strings.ToLower(strings.TrimSpace(target))
	switch {
	case strings.HasPrefix(lower, "data:image/"):
		return true
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return true
	case strings.HasPrefix(lower, "cliproxy-image:"):
		return true
	default:
		return false
	}
}

func isCodexSyntheticImageDisplayMessageID(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	return strings.HasSuffix(id, "_display") || strings.HasPrefix(id, "msg_ig_")
}

func codexHostedImageByIndex(images []codexHostedImage, idxStr string) (codexHostedImage, bool) {
	if len(images) == 0 {
		return codexHostedImage{}, false
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		return codexHostedImage{}, false
	}
	if idx >= len(images) {
		// Single-image turns often only emit 0.png; clamp overshoot to last result.
		idx = len(images) - 1
	}
	return images[idx], true
}

func codexImageMIMEFromFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	default:
		return "image/png"
	}
}

func sseJSONBody(payload []byte) []byte {
	if bytes.HasPrefix(payload, dataTag) {
		return bytes.TrimSpace(payload[len(dataTag):])
	}
	return payload
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
	mime := codexImageMIMEFromFormat(item.Get("output_format").String())
	// Markdown image so Desktop/agent markdown renderers can show the asset inline.
	// Fallback only: Desktop often drops this synthetic item; stream rewrite is primary.
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

// stripCodexHistoryDataURLImages removes multi-MB data:image payloads from request history
// before upstream. Desktop keeps outbound data-url markdown for display, then re-sends it
// on the next turn as text — that blows the model context (context_length_exceeded).
//
// Strategy (no server storage / no global cache):
//  1. Replace huge data URLs in history text with short cliproxy-image:N placeholders
//  2. Re-attach at most the last extracted image as a structured input_image on the latest
//     user turn so the model can re-identify / edit without keeping base64-as-text history
//  3. All image bytes come from the current request body and die with the request
func stripCodexHistoryDataURLImages(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// Fast path: nothing to do when payload has neither markdown data URLs nor a large
	// image_generation_call.result (Desktop usually only re-sends markdown data URLs).
	hasDataURL := bytes.Contains(body, []byte("data:image/"))
	hasIGResult := bytes.Contains(body, []byte(`"image_generation_call"`)) && bytes.Contains(body, []byte(`"result"`))
	if !hasDataURL && !hasIGResult {
		return body
	}
	seq := 0
	var lastImage *codexHistoryImage
	remember := func(dataURL string) {
		if img, ok := parseCodexDataURLImage(dataURL); ok {
			// Keep only the latest image from this request (bounded: 1).
			cp := img
			lastImage = &cp
		}
	}
	nextPlaceholder := func(alt string) string {
		seq++
		alt = strings.TrimSpace(alt)
		if alt == "" {
			alt = "generated image"
		}
		// Keep alt for model semantics; URL is a stable non-pixel ref.
		return fmt.Sprintf("![%s](cliproxy-image:%d)", alt, seq)
	}

	// Top-level string input (rare for Desktop).
	if in := gjson.GetBytes(body, "input"); in.Type == gjson.String {
		if next, ok := replaceCodexDataURLImagesInText(in.String(), nextPlaceholder, remember); ok {
			body, _ = sjson.SetBytes(body, "input", next)
		}
		// Cannot attach input_image to string input safely; placeholders only.
		return body
	}

	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	for itemIndex, item := range input.Array() {
		// Drop base64 from any re-sent image_generation_call history items; remember last.
		if strings.TrimSpace(item.Get("type").String()) == "image_generation_call" {
			if result := strings.TrimSpace(item.Get("result").String()); len(result) >= 256 {
				mime := codexImageMIMEFromFormat(item.Get("output_format").String())
				remember(fmt.Sprintf("data:%s;base64,%s", mime, result))
				path := fmt.Sprintf("input.%d.result", itemIndex)
				body, _ = sjson.DeleteBytes(body, path)
			}
			continue
		}
		if !hasDataURL {
			continue
		}
		// message / content text fields
		if content := item.Get("content"); content.Type == gjson.String {
			if next, ok := replaceCodexDataURLImagesInText(content.String(), nextPlaceholder, remember); ok {
				path := fmt.Sprintf("input.%d.content", itemIndex)
				body, _ = sjson.SetBytes(body, path, next)
			}
			continue
		}
		if content := item.Get("content"); content.IsArray() {
			for partIndex, part := range content.Array() {
				partType := strings.TrimSpace(part.Get("type").String())
				// Only rewrite text parts. Do not touch structured input_image (user uploads /
				// vision) — those are intentional pixels, not Desktop session replay of our
				// outbound markdown data URLs.
				if partType != "input_text" && partType != "output_text" && partType != "text" {
					continue
				}
				text := part.Get("text").String()
				if next, ok := replaceCodexDataURLImagesInText(text, nextPlaceholder, remember); ok {
					path := fmt.Sprintf("input.%d.content.%d.text", itemIndex, partIndex)
					body, _ = sjson.SetBytes(body, path, next)
				}
			}
		}
	}
	// Re-identify / edit: move last history image into structured input_image (not text tokens).
	if lastImage != nil {
		body = attachCodexHistoryImageToLastUser(body, *lastImage)
	}
	return body
}

func replaceCodexDataURLImagesInText(text string, nextPlaceholder func(alt string) string, remember func(dataURL string)) (string, bool) {
	if text == "" || !strings.Contains(text, "data:image/") {
		return text, false
	}
	out := text
	out = codexMarkdownDataURLImage.ReplaceAllStringFunc(out, func(match string) string {
		sub := codexMarkdownDataURLImage.FindStringSubmatch(match)
		alt := ""
		if len(sub) >= 2 {
			alt = sub[1]
		}
		if len(sub) >= 3 && remember != nil {
			remember(sub[2])
		}
		return nextPlaceholder(alt)
	})
	// Remaining bare data URLs (not wrapped in markdown).
	if strings.Contains(out, "data:image/") {
		out = codexBareDataURLImage.ReplaceAllStringFunc(out, func(match string) string {
			if remember != nil {
				remember(match)
			}
			return nextPlaceholder("generated image")
		})
	}
	if out == text {
		return text, false
	}
	return out, true
}

func parseCodexDataURLImage(dataURL string) (codexHistoryImage, bool) {
	dataURL = strings.TrimSpace(dataURL)
	if !strings.HasPrefix(dataURL, "data:image/") {
		return codexHistoryImage{}, false
	}
	// data:image/png;base64,<payload>
	rest := strings.TrimPrefix(dataURL, "data:")
	parts := strings.SplitN(rest, ",", 2)
	if len(parts) != 2 {
		return codexHistoryImage{}, false
	}
	meta, payload := parts[0], strings.TrimSpace(parts[1])
	if !strings.Contains(meta, "base64") || len(payload) < 256 {
		return codexHistoryImage{}, false
	}
	// Soft cap: refuse absurd single images to protect request path (no store, but still RAM).
	// 8MB base64 ≈ ~6MB binary — enough for normal Codex outputs, blocks pathological payloads.
	if len(payload) > 8<<20 {
		return codexHistoryImage{}, false
	}
	mime := strings.TrimSpace(strings.Split(meta, ";")[0])
	if mime == "" {
		mime = "image/png"
	}
	return codexHistoryImage{MIME: mime, Result: payload}, true
}

// attachCodexHistoryImageToLastUser adds at most one input_image to the latest user message.
// Image bytes are already in this request; we only restructure them for vision-capable models.
func attachCodexHistoryImageToLastUser(body []byte, img codexHistoryImage) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	items := input.Array()
	lastUser := -1
	for i := len(items) - 1; i >= 0; i-- {
		if strings.TrimSpace(items[i].Get("role").String()) == "user" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		return body
	}
	item := items[lastUser]
	// Skip if this user turn already has an input_image (user-uploaded).
	if content := item.Get("content"); content.IsArray() {
		for _, part := range content.Array() {
			if strings.TrimSpace(part.Get("type").String()) == "input_image" {
				return body
			}
		}
	}
	dataURL := fmt.Sprintf("data:%s;base64,%s", img.MIME, img.Result)
	imagePart := map[string]any{
		"type":      "input_image",
		"image_url": dataURL,
	}

	content := item.Get("content")
	if content.Type == gjson.String {
		// Promote string content to multipart so we can attach the image.
		parts := []any{
			map[string]any{"type": "input_text", "text": content.String()},
			imagePart,
		}
		path := fmt.Sprintf("input.%d.content", lastUser)
		body, _ = sjson.SetBytes(body, path, parts)
		return body
	}
	if content.IsArray() {
		path := fmt.Sprintf("input.%d.content.-1", lastUser)
		body, _ = sjson.SetBytes(body, path, imagePart)
		return body
	}
	// No content yet — create multipart.
	path := fmt.Sprintf("input.%d.content", lastUser)
	body, _ = sjson.SetBytes(body, path, []any{imagePart})
	return body
}
