// Package translator converts the three supported client protocols. It has no
// provider registration, credential handling, or dynamic extension mechanism.
package translator

import (
	"context"
	"github.com/tidwall/sjson"
	"sync"
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

type RequestTransform func(string, []byte, bool) []byte
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

var mu sync.RWMutex
var transforms = map[pair]transform{}

func Register(from, to Format, request RequestTransform, response ResponseTransform) {
	mu.Lock()
	defer mu.Unlock()
	transforms[pair{from, to}] = transform{request, response}
}
func Unregister(from, to Format) { mu.Lock(); defer mu.Unlock(); delete(transforms, pair{from, to}) }
func lookup(from, to Format) transform {
	mu.RLock()
	defer mu.RUnlock()
	return transforms[pair{from, to}]
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
func TranslateRequest(from, to Format, model string, body []byte, stream bool) []byte {
	if f := lookup(from, to).request; f != nil {
		return f(model, body, stream)
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
