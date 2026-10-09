package responses

import (
	. "github.com/1oo1/cpa-github-copilot/internal/constant"
	"github.com/1oo1/cpa-github-copilot/internal/protocol"
)

func init() {
	translator.RegisterBuiltin(
		OpenaiResponse,
		OpenAI,
		translator.AdaptRequestTransform(ConvertOpenAIResponsesRequestToOpenAIChatCompletions),
		translator.ResponseTransform{
			Stream:    ConvertOpenAIChatCompletionsResponseToOpenAIResponses,
			NonStream: ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream,
		},
	)
}
