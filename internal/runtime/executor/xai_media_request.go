package executor

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Request shaping for the Grok Imagine endpoints.
//
// The endpoints are OpenAI-compatible in shape but not in accepted arguments: xAI
// rejects the whole request with 400 "Argument not supported: size" when an
// OpenAI-only field is present. Forwarding the body verbatim therefore fails for
// any caller that sets size or quality — which the console does by default.
//
// An earlier revision passed media bodies through untranslated, reasoning that a
// translation layer would silently drop fields xAI might add later. That reasoning
// was wrong here: the risk is not hypothetical future fields, it is that today's
// request is rejected outright. Filtering to an allowlist keeps unknown fields out
// while leaving everything xAI documents intact.
//
// The allowlist mirrors the SupportedParameters declared for these models in
// internal/registry, so the catalog and the wire format cannot drift apart.

// xaiImageRequestFields are the top-level keys xAI accepts on /images/generations.
var xaiImageRequestFields = map[string]struct{}{
	"model":           {},
	"prompt":          {},
	"n":               {},
	"response_format": {},
	// Reference inputs for the edit endpoint; harmless on generations.
	"image": {},
	"mask":  {},
	// Passed through for upstream abuse attribution.
	"user": {},
}

// xaiDefaultResponseFormat is what the Imagine endpoints are asked for when the
// caller expressed no preference.
//
// xAI defaults to "url", which a Zero Data Retention team cannot use at all: the
// upstream rejects the request with "Zero Data Retention teams do not have access
// to URL format as it requires to store the generated images". Many subscription
// teams are ZDR, so the permissive default fails for them on every request.
// Requesting bytes back needs no server-side retention and works for both team
// types, and it is what the management console renders regardless.
//
// An explicit caller choice is preserved: a caller asking for "url" against a ZDR
// team gets the upstream error, which is the honest answer — that is a team
// setting, not something this proxy can work around.
const xaiDefaultResponseFormat = "b64_json"

// shapeXAIImageRequest removes fields the Grok Imagine API rejects.
//
// It returns the filtered body and the names it dropped, so the caller can tell the
// operator which of their settings were ignored rather than silently changing the
// meaning of the request.
func shapeXAIImageRequest(payload []byte) ([]byte, []string) {
	parsed := gjson.ParseBytes(payload)
	if !parsed.IsObject() {
		return payload, nil
	}

	dropped := make([]string, 0, 4)
	shaped := payload
	parsed.ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		if _, ok := xaiImageRequestFields[strings.ToLower(strings.TrimSpace(name))]; ok {
			return true
		}
		if next, err := sjson.DeleteBytes(shaped, escapeJSONPathKey(name)); err == nil {
			shaped = next
			dropped = append(dropped, name)
		}
		return true
	})

	shaped = withXAIDefaultResponseFormat(shaped)
	if len(dropped) == 0 && len(shaped) == len(payload) {
		return payload, nil
	}
	return shaped, dropped
}

// withXAIDefaultResponseFormat pins a response format when the caller omitted one.
func withXAIDefaultResponseFormat(payload []byte) []byte {
	if strings.TrimSpace(gjson.GetBytes(payload, "response_format").String()) != "" {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "response_format", xaiDefaultResponseFormat)
	if err != nil {
		return payload
	}
	return updated
}

// escapeJSONPathKey protects keys containing the separators sjson treats specially,
// so a field named "a.b" is deleted rather than being read as a nested path.
func escapeJSONPathKey(key string) string {
	replacer := strings.NewReplacer(".", `\.`, "*", `\*`, "?", `\?`)
	return replacer.Replace(key)
}
