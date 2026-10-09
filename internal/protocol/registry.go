// Package translator converts the supported client protocols. Its transform
// table contains compile-time builtins only and has no dynamic extension path.
package translator

import (
	"context"
	"sync/atomic"

	"github.com/tidwall/sjson"
)

type Format string

func (f Format) String() string  { return string(f) }
func FromString(s string) Format { return Format(s) }

const (
	FormatOpenAI         Format = "openai"
	FormatOpenAIResponse Format = "openai-response"
	FormatClaude         Format = "claude"
	FormatCodex          Format = "codex"
)

type ModelCapabilities struct {
	MaxOutputTokens  int64
	AdaptiveThinking bool
	ThinkingLevels   []string
}

type RequestTransform func(string, ModelCapabilities, []byte, bool) []byte
type RequestTransformWithoutCapabilities func(string, []byte, bool) []byte
type ResponseStreamTransform func(context.Context, string, []byte, []byte, []byte, *any) [][]byte
type ResponseNonStreamTransform func(context.Context, string, []byte, []byte, []byte, *any) []byte
type ResponseTokenCountTransform func(context.Context, int64) []byte
type ResponseTransform struct {
	Stream     ResponseStreamTransform
	NonStream  ResponseNonStreamTransform
	TokenCount ResponseTokenCountTransform
}
type pair struct{ from, to Format }
type transform struct {
	request  RequestTransform
	response ResponseTransform
}

func AdaptRequestTransform(fn RequestTransformWithoutCapabilities) RequestTransform {
	return func(model string, _ ModelCapabilities, body []byte, stream bool) []byte {
		return fn(model, body, stream)
	}
}

var transforms atomic.Pointer[map[pair]transform]

func RegisterBuiltin(from, to Format, request RequestTransform, response ResponseTransform) {
	previous := transforms.Load()
	next := make(map[pair]transform)
	if previous != nil {
		for key, value := range *previous {
			next[key] = value
		}
	}
	next[pair{from, to}] = transform{request, response}
	transforms.Store(&next)
}
func lookup(from, to Format) transform {
	table := transforms.Load()
	if table == nil {
		return transform{}
	}
	return (*table)[pair{from, to}]
}
func HasRequestTransformer(from, to Format) bool { return lookup(from, to).request != nil }
func HasResponseTransformer(from, to Format) bool {
	return HasStreamResponseTransformer(from, to) || HasNonStreamResponseTransformer(from, to)
}
func HasStreamResponseTransformer(from, to Format) bool {
	return lookup(from, to).response.Stream != nil
}
func HasNonStreamResponseTransformer(from, to Format) bool {
	return lookup(from, to).response.NonStream != nil
}
func TranslateRequest(from, to Format, model string, capabilities ModelCapabilities, body []byte, stream bool) []byte {
	if f := lookup(from, to).request; f != nil {
		return f(model, capabilities, body, stream)
	}
	out, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return body
	}
	return out
}
func TranslateStream(ctx context.Context, from, to Format, model string, original, request, body []byte, param *any) [][]byte {
	if f := lookup(to, from).response.Stream; f != nil {
		return f(ctx, model, original, request, body, param)
	}
	return [][]byte{body}
}
func TranslateNonStream(ctx context.Context, from, to Format, model string, original, request, body []byte, param *any) []byte {
	if f := lookup(to, from).response.NonStream; f != nil {
		return f(ctx, model, original, request, body, param)
	}
	return body
}
