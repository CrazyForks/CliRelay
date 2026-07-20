package util

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestMutateTopLevelObject_SetDeletePreserve(t *testing.T) {
	src := []byte(`{"model":"gpt","input":[{"role":"user"}],"user":"u","temperature":0.2}`)
	out := MutateTopLevelObject(src, map[string][]byte{
		"stream":              []byte("true"),
		"store":               []byte("false"),
		"parallel_tool_calls": []byte("true"),
		"include":             []byte(`["reasoning.encrypted_content"]`),
	}, []string{"user", "temperature", "max_output_tokens"})

	if gjson.GetBytes(out, "model").String() != "gpt" {
		t.Fatalf("model lost: %s", out)
	}
	if !gjson.GetBytes(out, "stream").Bool() {
		t.Fatalf("stream not set: %s", out)
	}
	if gjson.GetBytes(out, "store").Bool() {
		t.Fatalf("store not false: %s", out)
	}
	if gjson.GetBytes(out, "user").Exists() || gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("deleted keys remain: %s", out)
	}
	if got := gjson.GetBytes(out, "input.0.role").String(); got != "user" {
		t.Fatalf("input corrupted: %s", out)
	}
	if got := gjson.GetBytes(out, "include.0").String(); got != "reasoning.encrypted_content" {
		t.Fatalf("include wrong: %s", out)
	}
}

func TestMutateTopLevelObject_ReplaceExisting(t *testing.T) {
	src := []byte(`{"stream":false,"input":[1,2,3]}`)
	out := MutateTopLevelObject(src, map[string][]byte{
		"stream": []byte("true"),
		"input":  []byte(`[{"role":"developer"}]`),
	}, nil)
	if !gjson.GetBytes(out, "stream").Bool() {
		t.Fatalf("stream: %s", out)
	}
	if got := gjson.GetBytes(out, "input.0.role").String(); got != "developer" {
		t.Fatalf("input: %s", out)
	}
}
