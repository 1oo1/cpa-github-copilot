package registry

import "sync"

type ThinkingSupport struct {
	Min            int
	Max            int
	ZeroAllowed    bool
	DynamicAllowed bool
	Levels         []string
}
type ModelInfo struct {
	ID                  string
	Thinking            *ThinkingSupport
	MaxCompletionTokens int
	ContextLength       int
	SupportedParameters []string
}

var mu sync.RWMutex
var models = map[string]*ModelInfo{}

func LookupModelInfo(id string, _ ...string) *ModelInfo {
	mu.RLock()
	defer mu.RUnlock()
	return models[id]
}
func SetModel(info *ModelInfo) { mu.Lock(); defer mu.Unlock(); models[info.ID] = info }

func RemoveModel(id string) { mu.Lock(); defer mu.Unlock(); delete(models, id) }
