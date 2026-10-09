package responses

import (
	. "github.com/1oo1/cpa-github-copilot/internal/constant"
	"github.com/1oo1/cpa-github-copilot/internal/protocol"
)

func init() {
	translator.RegisterBuiltin(
		OpenaiResponse,
		Claude,
		func(model string, capabilities translator.ModelCapabilities, body []byte, stream bool) []byte {
			return ConvertOpenAIResponsesRequestToClaudeWithCapabilities(model, capabilities, body, stream)
		},
		translator.ResponseTransform{
			Stream:    ConvertClaudeResponseToOpenAIResponses,
			NonStream: ConvertClaudeResponseToOpenAIResponsesNonStream,
		},
	)
}
