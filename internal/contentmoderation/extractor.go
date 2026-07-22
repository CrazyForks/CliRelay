package contentmoderation

import (
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func ExtractLastUserText(format sdktranslator.Format, payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	var parts []string
	switch format {
	case sdktranslator.FormatOpenAIResponse:
		collectResponsesInput(gjson.GetBytes(payload, "input"), &parts)
	case sdktranslator.FormatClaude:
		collectLastRoleContent(gjson.GetBytes(payload, "messages"), "user", &parts, true)
	case sdktranslator.FormatGemini, sdktranslator.FormatGeminiCLI:
		collectLastGeminiContent(gjson.GetBytes(payload, "contents"), &parts)
	default:
		collectLastRoleContent(gjson.GetBytes(payload, "messages"), "user", &parts, false)
	}
	return strings.Join(strings.Fields(strings.Join(parts, "\n")), " ")
}

func collectLastRoleContent(messages gjson.Result, role string, parts *[]string, skipSystemReminder bool) {
	if !messages.IsArray() {
		return
	}
	items := messages.Array()
	if len(items) == 0 {
		return
	}
	last := items[len(items)-1]
	if !strings.EqualFold(strings.TrimSpace(last.Get("role").String()), role) {
		return
	}
	collectTextValue(last.Get("content"), parts, skipSystemReminder)
}

func collectResponsesInput(input gjson.Result, parts *[]string) {
	switch {
	case !input.Exists():
		return
	case input.Type == gjson.String:
		addText(parts, input.String(), false)
	case input.IsArray():
		items := input.Array()
		if len(items) == 0 {
			return
		}
		last := items[len(items)-1]
		role := strings.ToLower(strings.TrimSpace(last.Get("role").String()))
		typeName := strings.ToLower(strings.TrimSpace(last.Get("type").String()))
		if role != "user" && typeName != "input_text" {
			return
		}
		collectTextValue(last.Get("content"), parts, false)
		if typeName == "input_text" || last.Get("text").Exists() {
			collectTextValue(last, parts, false)
		}
	case input.IsObject():
		role := strings.ToLower(strings.TrimSpace(input.Get("role").String()))
		typeName := strings.ToLower(strings.TrimSpace(input.Get("type").String()))
		if role == "user" || typeName == "input_text" {
			collectTextValue(input.Get("content"), parts, false)
			collectTextValue(input, parts, false)
		}
	}
}

func collectLastGeminiContent(contents gjson.Result, parts *[]string) {
	if !contents.IsArray() {
		return
	}
	items := contents.Array()
	if len(items) == 0 {
		return
	}
	last := items[len(items)-1]
	role := strings.ToLower(strings.TrimSpace(last.Get("role").String()))
	if role != "" && role != "user" {
		return
	}
	if values := last.Get("parts"); values.IsArray() {
		for _, part := range values.Array() {
			addText(parts, part.Get("text").String(), false)
		}
	}
}

func collectTextValue(value gjson.Result, parts *[]string, skipSystemReminder bool) {
	switch {
	case !value.Exists():
		return
	case value.Type == gjson.String:
		addText(parts, value.String(), skipSystemReminder)
	case value.IsArray():
		for _, item := range value.Array() {
			collectTextValue(item, parts, skipSystemReminder)
		}
	case value.IsObject():
		typeName := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
		if typeName == "" || typeName == "text" || typeName == "input_text" || typeName == "message" {
			addText(parts, value.Get("text").String(), skipSystemReminder)
			collectTextValue(value.Get("content"), parts, skipSystemReminder)
		}
	}
}

func addText(parts *[]string, value string, skipSystemReminder bool) {
	value = strings.TrimSpace(value)
	if value == "" || (skipSystemReminder && strings.HasPrefix(value, "<system-reminder>")) {
		return
	}
	*parts = append(*parts, value)
}
