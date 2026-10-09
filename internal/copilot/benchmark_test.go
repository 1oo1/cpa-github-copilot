package copilot

import (
	"bytes"
	"testing"
)

func BenchmarkNormalizeInferencePayloadForRoute(b *testing.B) {
	raw := []byte(`{
		"model":"claude-test",
		"max_tokens":100000,
		"system":[{"type":"text","text":"synthetic system prompt"}],
		"thinking":{"type":"enabled","budget_tokens":16000},
		"output_config":{"effort":"high"},
		"tools":[{"name":"lookup","description":"synthetic tool","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"synthetic benchmark prompt"}]
	}`)
	route := modelRoute{
		Format:                  formatClaude,
		Path:                    "/v1/messages",
		AdaptiveThinking:        true,
		MaxOutputTokens:         64000,
		ReasoningLevels:         []string{"low", "medium", "high"},
		ReasoningLevelsDeclared: true,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := normalizeInferencePayloadForRoute(raw, "claude-test", route, false, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSSEFramerPush(b *testing.B) {
	chunk := []byte("data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"synthetic\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"payload\"}}\n\n")
	frames := bytes.Count(chunk, []byte("\n\n"))
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		framer := newSSEFramer(0)
		got, err := framer.Push(chunk)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != frames {
			b.Fatalf("frames = %d, want %d", len(got), frames)
		}
	}
}
