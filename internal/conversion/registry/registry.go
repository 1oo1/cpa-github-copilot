package translator

import (
	"github.com/1oo1/cpa-github-copilot/internal/interfaces"
	t "github.com/1oo1/cpa-github-copilot/internal/protocol"
)

func Register(from, to string, request interfaces.TranslateRequestFunc, response interfaces.TranslateResponse) {
	t.Register(t.Format(from), t.Format(to), request, response)
}
