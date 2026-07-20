package claude

import (
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodex_LargeHistoryAllocBound(t *testing.T) {
	// Multi-MB multi-turn history: old path did sjson.SetRaw(template,"input.-1") per message.
	var b strings.Builder
	b.WriteString(`{"model":"claude-sonnet-4-6","messages":[`)
	chunk := strings.Repeat("x", 64*1024)
	for i := 0; i < 32; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		b.WriteString(`{"role":"`)
		b.WriteString(role)
		b.WriteString(`","content":[{"type":"text","text":"`)
		b.WriteString(chunk)
		b.WriteString(`"}]}`)
	}
	b.WriteString(`]}`)
	in := []byte(b.String())

	out := ConvertClaudeRequestToCodex("gpt-5.4", in, true)
	if got := gjson.GetBytes(out, "input.#").Int(); got < 32 {
		t.Fatalf("input items = %d, want >=32", got)
	}
	if !gjson.GetBytes(out, "stream").Bool() {
		t.Fatalf("stream not set")
	}

	res := testing.Benchmark(func(bb *testing.B) {
		bb.ReportAllocs()
		bb.SetBytes(int64(len(in)))
		for i := 0; i < bb.N; i++ {
			_ = ConvertClaudeRequestToCodex("gpt-5.4", in, true)
		}
	})
	if res.N == 0 {
		t.Fatal("benchmark did not run")
	}
	avg := res.AllocedBytesPerOp()
	// One pass rebuild (no growing-template SetRaw). Old N×full-body path was far higher.
	if avg > int64(len(in))*12 {
		t.Fatalf("alloc/op=%d body=%d ratio=%.1f want <=12x", avg, len(in), float64(avg)/float64(len(in)))
	}
	t.Logf("body=%dB alloc/op=%d N=%d ratio=%.2f", len(in), avg, res.N, float64(avg)/float64(len(in)))
}

func TestConvertClaudeRequestToCodex_DeferLoadingToolsAllocNotLinearInBody(t *testing.T) {
	// Many deferred tools must not copy the full multi-MB body once per tool.
	chunk := strings.Repeat("y", 128*1024)
	var b strings.Builder
	b.WriteString(`{"model":"claude-sonnet-4-6","messages":[`)
	for i := 0; i < 16; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		b.WriteString(`{"role":"`)
		b.WriteString(role)
		b.WriteString(`","content":[{"type":"text","text":"`)
		b.WriteString(chunk)
		b.WriteString(`"}]}`)
	}
	b.WriteString(`],"tools":[`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":"Tool`)
		b.WriteString(strconv.Itoa(i))
		b.WriteString(`","description":"d","input_schema":{"type":"object"},"defer_loading":true}`)
	}
	b.WriteString(`]}`)
	in := []byte(b.String())

	out := ConvertClaudeRequestToCodex("gpt-5.4", in, true)
	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() || len(tools.Array()) != 50 {
		t.Fatalf("tools = %d, want 50", len(tools.Array()))
	}
	tools.ForEach(func(i, tool gjson.Result) bool {
		if tool.Get("defer_loading").Exists() {
			t.Fatalf("tool %d still has defer_loading", i.Int())
		}
		return true
	})

	res := testing.Benchmark(func(bb *testing.B) {
		bb.ReportAllocs()
		bb.SetBytes(int64(len(in)))
		for i := 0; i < bb.N; i++ {
			_ = ConvertClaudeRequestToCodex("gpt-5.4", in, true)
		}
	})
	if res.N == 0 {
		t.Fatal("benchmark did not run")
	}
	avg := res.AllocedBytesPerOp()
	// Old path: ~toolCount full-body copies. Cap well below 50× body.
	if avg > int64(len(in))*20 {
		t.Fatalf("alloc/op=%d body=%d ratio=%.1f want <=20x with 50 deferred tools", avg, len(in), float64(avg)/float64(len(in)))
	}
	t.Logf("body=%dB tools=50 alloc/op=%d ratio=%.2f", len(in), avg, float64(avg)/float64(len(in)))
}
