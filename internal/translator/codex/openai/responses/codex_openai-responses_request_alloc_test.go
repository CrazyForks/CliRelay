package responses

import (
	"strings"
	"testing"
)

func TestConvertOpenAIResponsesRequestToCodex_LargeBodyAllocBound(t *testing.T) {
	// Multi-MB-ish history: old path did N full sjson copies; new path should stay O(body size).
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.4","stream":false,"user":"u","temperature":0.2,"max_output_tokens":100,"input":[`)
	// ~2MB of text across messages
	chunk := strings.Repeat("x", 64*1024)
	for i := 0; i < 32; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		role := "user"
		if i == 0 {
			role = "system"
		}
		b.WriteString(`{"type":"message","role":"`)
		b.WriteString(role)
		b.WriteString(`","content":[{"type":"input_text","text":"`)
		b.WriteString(chunk)
		b.WriteString(`"}]}`)
	}
	b.WriteString(`]}`)
	in := []byte(b.String())

	var totalAlloc uint64
	const N = 5
	res := testing.Benchmark(func(bb *testing.B) {
		bb.ReportAllocs()
		bb.SetBytes(int64(len(in)))
		for i := 0; i < bb.N; i++ {
			out := ConvertOpenAIResponsesRequestToCodex("gpt-5.4", in, true)
			if len(out) < len(in)/2 {
				bb.Fatalf("output too small: %d", len(out))
			}
		}
	})
	// Rough ceiling: more than ~8 full body copies per op is a regression of the old N*sjson path.
	if res.N > 0 {
		avg := res.AllocedBytesPerOp()
		totalAlloc = uint64(avg)
		if avg > int64(len(in))*8 {
			t.Fatalf("alloc/op=%d body=%d ratio=%.1f want <=8x body", avg, len(in), float64(avg)/float64(len(in)))
		}
	}
	t.Logf("body=%dB alloc/op=%dN=%d", len(in), totalAlloc, res.N)
}
