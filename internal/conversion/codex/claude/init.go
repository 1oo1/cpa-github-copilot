package claude

import (
	. "github.com/1oo1/cpa-github-copilot/internal/constant"
	"github.com/1oo1/cpa-github-copilot/internal/protocol"
)

func init() {
	translator.RegisterBuiltin(
		Claude,
		Codex,
		translator.AdaptRequestTransform(ConvertClaudeRequestToCodex),
		translator.ResponseTransform{
			Stream:     ConvertCodexResponseToClaude,
			NonStream:  ConvertCodexResponseToClaudeNonStream,
			TokenCount: ClaudeTokenCount,
		},
	)
}
