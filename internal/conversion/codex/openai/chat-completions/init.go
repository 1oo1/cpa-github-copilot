package chat_completions

import (
	. "github.com/1oo1/cpa-github-copilot/internal/constant"
	"github.com/1oo1/cpa-github-copilot/internal/protocol"
)

func init() {
	translator.RegisterBuiltin(
		OpenAI,
		Codex,
		translator.AdaptRequestTransform(ConvertOpenAIRequestToCodex),
		translator.ResponseTransform{
			Stream:    ConvertCodexResponseToOpenAI,
			NonStream: ConvertCodexResponseToOpenAINonStream,
		},
	)
}
