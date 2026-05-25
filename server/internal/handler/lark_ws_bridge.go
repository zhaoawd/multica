package handler

import (
	"context"

	"github.com/multica-ai/multica/server/internal/service"
)

// NewLarkWSMessageHandler returns the callback for LarkWSClient that
// bridges SDK message events into the existing ProcessLarkMessageEvent
// path. This keeps the WebSocket integration zero-change for all
// downstream business logic.
func (h *Handler) NewLarkWSMessageHandler() service.LarkMessageEventHandler {
	return func(ctx context.Context, sender service.LarkWSSender, msg service.LarkWSMessage) {
		evt := larkMessageEvent{}
		evt.Event.Sender.SenderType = sender.SenderType
		evt.Event.Sender.SenderID.OpenID = sender.OpenID
		evt.Event.Message.MessageID = msg.MessageID
		evt.Event.Message.RootID = msg.RootID
		evt.Event.Message.ParentID = msg.ParentID
		evt.Event.Message.ThreadID = msg.ThreadID
		evt.Event.Message.ChatID = msg.ChatID
		evt.Event.Message.ChatType = msg.ChatType
		evt.Event.Message.MsgType = msg.MsgType
		evt.Event.Message.Content = msg.Content
		evt.Event.Message.CreateTime = msg.CreateTime
		h.ProcessLarkMessageEvent(ctx, evt)
	}
}

// NewLarkWSCardActionHandler returns the callback for LarkWSClient that
// bridges SDK card action callbacks into the existing
// processLarkCardAction path.
func (h *Handler) NewLarkWSCardActionHandler() service.LarkCardActionHandler {
	return func(ctx context.Context, openID string, value map[string]any) (string, string) {
		toast := h.processLarkCardAction(ctx, openID, value)
		return toast.Toast.Type, toast.Toast.Content
	}
}
