package chat_completions

import (
	. "github.com/1oo1/cpa-github-copilot/internal/constant"
	"github.com/1oo1/cpa-github-copilot/internal/protocol"
)

func init() {
	translator.RegisterBuiltin(
		OpenAI,
		Claude,
		func(model string, capabilities translator.ModelCapabilities, body []byte, stream bool) []byte {
			return ConvertOpenAIRequestToClaudeWithCapabilities(model, capabilities, body, stream)
		},
		translator.ResponseTransform{
			Stream:    ConvertClaudeResponseToOpenAI,
			NonStream: ConvertClaudeResponseToOpenAINonStream,
		},
	)
}
