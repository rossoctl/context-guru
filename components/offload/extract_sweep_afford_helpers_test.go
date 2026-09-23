package offload

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func schemaText(req *bschemas.BifrostChatRequest, i int) string {
	return schema.MessageText(req.Input[i])
}

func setMessageTextAt(req *bschemas.BifrostChatRequest, i int, s string) {
	schema.SetMessageText(&req.Input[i], s)
}

func newMemStore() store.Store { return store.NewMemory(store.Options{}) }
