package chat_completions

import (
	. "github.com/1oo1/cpa-github-copilot/internal/constant"
	"github.com/1oo1/cpa-github-copilot/internal/protocol"
)

func init() {
	translator.RegisterBuiltin(
		OpenAI,
		OpenAI,
		translator.AdaptRequestTransform(ConvertOpenAIRequestToOpenAI),
		translator.ResponseTransform{
			Stream:    ConvertOpenAIResponseToOpenAI,
			NonStream: ConvertOpenAIResponseToOpenAINonStream,
		},
	)
}
