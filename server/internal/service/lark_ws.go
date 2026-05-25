package service

import (
	"context"
	"log/slog"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// LarkMessageEventHandler is the function signature the WebSocket client
// calls when an im.message.receive_v1 event arrives. The handler layer
// implements this — see Handler.ProcessLarkMessageEvent.
type LarkMessageEventHandler func(ctx context.Context, sender LarkWSSender, msg LarkWSMessage)

// LarkCardActionHandler is the function signature the WebSocket client
// calls when a card.action.trigger callback arrives. Returns a toast
// response that the SDK sends back to Lark over the WebSocket.
type LarkCardActionHandler func(ctx context.Context, openID string, value map[string]any) (toastType, toastContent string)

// LarkWSSender mirrors the sender identity fields from the SDK event.
type LarkWSSender struct {
	SenderType string
	OpenID     string
}

// LarkWSMessage mirrors the message fields the handler layer needs.
type LarkWSMessage struct {
	MessageID  string
	RootID     string
	ParentID   string
	ThreadID   string
	ChatID     string
	ChatType   string
	MsgType    string
	Content    string
	CreateTime string
}

// LarkWSClient wraps the oapi-sdk-go WebSocket client and bridges
// incoming events to the existing handler layer. The server starts
// this when LARK_CALLBACK_MODE=websocket.
type LarkWSClient struct {
	wsClient     *larkws.Client
	onMessage    LarkMessageEventHandler
	onCardAction LarkCardActionHandler
	cancelFunc   context.CancelFunc
}

// NewLarkWSClient creates a WebSocket long-connection client that
// receives Lark events without requiring a public callback URL.
//
// onMessage is called for im.message.receive_v1 events (@bot messages,
// slash commands, thread replies). onCardAction is registered for
// card.action.trigger but note that the oapi-sdk-go v3.9.2 WS client
// does NOT dispatch MessageTypeCard frames (ws/client.go line 624).
// Card button callbacks must still flow through the HTTP webhook
// endpoint. The handler is registered here so it will work if a future
// SDK version adds card dispatch support.
func NewLarkWSClient(
	cfg LarkConfig,
	onMessage LarkMessageEventHandler,
	onCardAction LarkCardActionHandler,
) *LarkWSClient {
	d := dispatcher.NewEventDispatcher(cfg.VerificationToken, cfg.EncryptKey)

	lws := &LarkWSClient{
		onMessage:    onMessage,
		onCardAction: onCardAction,
	}

	d.OnP2MessageReceiveV1(lws.handleMessageReceive)

	d.OnP2CardActionTrigger(lws.handleCardAction)

	wsClient := larkws.NewClient(
		cfg.AppID,
		cfg.AppSecret,
		larkws.WithEventHandler(d),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithAutoReconnect(true),
		larkws.WithOnReady(func() {
			slog.Info("lark websocket: connected and ready")
		}),
		larkws.WithOnReconnecting(func() {
			slog.Warn("lark websocket: reconnecting")
		}),
		larkws.WithOnReconnected(func() {
			slog.Info("lark websocket: reconnected")
		}),
		larkws.WithOnDisconnected(func() {
			slog.Warn("lark websocket: disconnected")
		}),
		larkws.WithOnError(func(err error) {
			slog.Error("lark websocket: error", "err", err)
		}),
	)

	lws.wsClient = wsClient
	return lws
}

// Start connects to Lark and blocks forever. The underlying SDK uses
// `select {}` after connecting, so ctx cancellation does not cause
// Start to return. Use Close to disconnect the WebSocket; the Start
// goroutine will remain blocked but is harmless at process exit.
// Should be called in a goroutine.
func (c *LarkWSClient) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancelFunc = cancel
	return c.wsClient.Start(ctx)
}

// Close disconnects the WebSocket. The underlying SDK sets
// autoReconnect=false and closes the connection, causing the receive
// loop to exit. The Start goroutine remains blocked on `select {}`
// which is an SDK limitation; it does not affect process shutdown.
func (c *LarkWSClient) Close() {
	if c.cancelFunc != nil {
		c.cancelFunc()
	}
	c.wsClient.Close()
}

func (c *LarkWSClient) handleMessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if c.onMessage == nil || event == nil || event.Event == nil {
		return nil
	}

	sender := LarkWSSender{}
	if event.Event.Sender != nil {
		if event.Event.Sender.SenderType != nil {
			sender.SenderType = *event.Event.Sender.SenderType
		}
		if event.Event.Sender.SenderId != nil && event.Event.Sender.SenderId.OpenId != nil {
			sender.OpenID = *event.Event.Sender.SenderId.OpenId
		}
	}

	msg := LarkWSMessage{}
	if m := event.Event.Message; m != nil {
		if m.MessageId != nil {
			msg.MessageID = *m.MessageId
		}
		if m.RootId != nil {
			msg.RootID = *m.RootId
		}
		if m.ParentId != nil {
			msg.ParentID = *m.ParentId
		}
		if m.ThreadId != nil {
			msg.ThreadID = *m.ThreadId
		}
		if m.ChatId != nil {
			msg.ChatID = *m.ChatId
		}
		if m.ChatType != nil {
			msg.ChatType = *m.ChatType
		}
		if m.MessageType != nil {
			msg.MsgType = *m.MessageType
		}
		if m.Content != nil {
			msg.Content = *m.Content
		}
		if m.CreateTime != nil {
			msg.CreateTime = *m.CreateTime
		}
	}

	c.onMessage(ctx, sender, msg)
	return nil
}

func (c *LarkWSClient) handleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if c.onCardAction == nil || event == nil || event.Event == nil {
		return nil, nil
	}

	if event.Event.Action == nil || event.Event.Action.Tag != "button" {
		return nil, nil
	}

	openID := ""
	if event.Event.Operator != nil {
		openID = event.Event.Operator.OpenID
	}

	toastType, toastContent := c.onCardAction(ctx, openID, event.Event.Action.Value)

	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{
			Type:    toastType,
			Content: toastContent,
		},
	}, nil
}
