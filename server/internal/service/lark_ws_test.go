package service

import (
	"context"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// LarkWSClient bridges Lark's SDK long-connection events into the
// transport-agnostic ProcessLarkMessageEvent / processLarkCardAction
// paths. These tests pin the field-mapping fidelity and nil-guard
// behavior on the two private dispatch methods so a future SDK
// upgrade can't silently drop a field on the floor.
//
// We construct LarkWSClient literals (skipping NewLarkWSClient, which
// initializes a real websocket dialer) — only the onMessage /
// onCardAction callbacks are exercised here.

// ptr returns a pointer to s. SDK message fields are *string and a
// nil pointer means "field absent", which is the only way to verify
// the handlers correctly avoid dereferencing missing fields.
func ptr(s string) *string { return &s }

// ── handleMessageReceive ─────────────────────────────────────────────────

func TestLarkWS_HandleMessageReceive_FieldMapping_AllFieldsPropagate(t *testing.T) {
	var got LarkWSMessage
	var gotSender LarkWSSender
	called := false
	lws := &LarkWSClient{
		onMessage: func(_ context.Context, s LarkWSSender, m LarkWSMessage) {
			called = true
			gotSender = s
			got = m
		},
	}

	evt := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: ptr("user"),
				SenderId:   &larkim.UserId{OpenId: ptr("ou_alice")},
			},
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_msg_1"),
				RootId:      ptr("om_root_1"),
				ParentId:    ptr("om_parent_1"),
				ThreadId:    ptr("om_thread_1"),
				ChatId:      ptr("oc_chat_1"),
				ChatType:    ptr("group"),
				MessageType: ptr("text"),
				Content:     ptr(`{"text":"hello"}`),
				CreateTime:  ptr("1747555200000"),
			},
		},
	}
	if err := lws.handleMessageReceive(context.Background(), evt); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !called {
		t.Fatal("onMessage callback was not invoked")
	}
	if gotSender.SenderType != "user" || gotSender.OpenID != "ou_alice" {
		t.Errorf("sender: %+v", gotSender)
	}
	want := LarkWSMessage{
		MessageID:  "om_msg_1",
		RootID:     "om_root_1",
		ParentID:   "om_parent_1",
		ThreadID:   "om_thread_1",
		ChatID:     "oc_chat_1",
		ChatType:   "group",
		MsgType:    "text",
		Content:    `{"text":"hello"}`,
		CreateTime: "1747555200000",
	}
	if got != want {
		t.Errorf("message mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestLarkWS_HandleMessageReceive_NilCallback_NoPanic(t *testing.T) {
	lws := &LarkWSClient{onMessage: nil}
	evt := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{MessageId: ptr("om_1")},
		},
	}
	if err := lws.handleMessageReceive(context.Background(), evt); err != nil {
		t.Errorf("nil onMessage should be silently skipped; got err %v", err)
	}
}

func TestLarkWS_HandleMessageReceive_NilEvent_NoPanic(t *testing.T) {
	called := false
	lws := &LarkWSClient{onMessage: func(_ context.Context, _ LarkWSSender, _ LarkWSMessage) {
		called = true
	}}
	if err := lws.handleMessageReceive(context.Background(), nil); err != nil {
		t.Errorf("nil event should be silently skipped; got err %v", err)
	}
	if called {
		t.Error("callback must not fire when event is nil")
	}
}

func TestLarkWS_HandleMessageReceive_NilInnerEvent_NoPanic(t *testing.T) {
	called := false
	lws := &LarkWSClient{onMessage: func(_ context.Context, _ LarkWSSender, _ LarkWSMessage) {
		called = true
	}}
	evt := &larkim.P2MessageReceiveV1{Event: nil}
	if err := lws.handleMessageReceive(context.Background(), evt); err != nil {
		t.Errorf("nil inner event should be silently skipped; got err %v", err)
	}
	if called {
		t.Error("callback must not fire when inner Event is nil")
	}
}

func TestLarkWS_HandleMessageReceive_PartialFields_ZeroOnAbsent(t *testing.T) {
	// Sender absent, Message present but only MessageId set. The handler
	// must still fire (sender_type filter lives downstream in
	// ProcessLarkMessageEvent, not here), and every absent field must
	// land as the empty string — not panic on nil deref.
	var got LarkWSMessage
	var gotSender LarkWSSender
	called := false
	lws := &LarkWSClient{
		onMessage: func(_ context.Context, s LarkWSSender, m LarkWSMessage) {
			called = true
			gotSender = s
			got = m
		},
	}
	evt := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender:  nil,
			Message: &larkim.EventMessage{MessageId: ptr("om_only")},
		},
	}
	if err := lws.handleMessageReceive(context.Background(), evt); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Fatal("callback should fire even with partial fields")
	}
	if gotSender.SenderType != "" || gotSender.OpenID != "" {
		t.Errorf("nil sender should produce zero LarkWSSender; got %+v", gotSender)
	}
	if got.MessageID != "om_only" {
		t.Errorf("MessageID = %q, want om_only", got.MessageID)
	}
	if got.ChatID != "" || got.ThreadID != "" || got.MsgType != "" {
		t.Errorf("absent fields must be empty strings; got %+v", got)
	}
}

func TestLarkWS_HandleMessageReceive_SenderIDPresent_OpenIDNil_NoPanic(t *testing.T) {
	// SenderId object is present but its OpenId pointer is nil — the
	// real SDK does this when only union_id is delivered. Don't crash;
	// just leave OpenID empty.
	called := false
	lws := &LarkWSClient{onMessage: func(_ context.Context, s LarkWSSender, _ LarkWSMessage) {
		called = true
		if s.OpenID != "" {
			t.Errorf("expected empty OpenID; got %q", s.OpenID)
		}
	}}
	evt := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderType: ptr("user"),
				SenderId:   &larkim.UserId{OpenId: nil},
			},
			Message: &larkim.EventMessage{MessageId: ptr("om_x"), ChatId: ptr("oc_x")},
		},
	}
	if err := lws.handleMessageReceive(context.Background(), evt); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Error("callback should still fire")
	}
}

// ── handleCardAction ─────────────────────────────────────────────────────

func TestLarkWS_HandleCardAction_FieldMapping_WrapsToast(t *testing.T) {
	var gotOpenID string
	var gotValue map[string]any
	lws := &LarkWSClient{
		onCardAction: func(_ context.Context, openID string, value map[string]any) (string, string) {
			gotOpenID = openID
			gotValue = value
			return "success", "Claimed"
		},
	}
	evt := &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_clicker"},
			Action: &callback.CallBackAction{
				Tag:   "button",
				Value: map[string]any{"verb": "claim", "issue_id": "issue-uuid"},
			},
		},
	}
	resp, err := lws.handleCardAction(context.Background(), evt)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotOpenID != "ou_clicker" {
		t.Errorf("openID = %q", gotOpenID)
	}
	if v, _ := gotValue["verb"].(string); v != "claim" {
		t.Errorf("value.verb = %v", gotValue["verb"])
	}
	if resp == nil || resp.Toast == nil {
		t.Fatal("expected toast wrapped in response")
	}
	if resp.Toast.Type != "success" || resp.Toast.Content != "Claimed" {
		t.Errorf("toast = %+v", resp.Toast)
	}
}

func TestLarkWS_HandleCardAction_NonButtonTag_DoesNotInvokeHandler(t *testing.T) {
	// Lark also delivers select/input/etc. trigger events. The bridge
	// only services button clicks (verbs come from button Value maps),
	// so other tags must be a no-op — no toast, no handler call.
	called := false
	lws := &LarkWSClient{
		onCardAction: func(_ context.Context, _ string, _ map[string]any) (string, string) {
			called = true
			return "should", "not happen"
		},
	}
	for _, tag := range []string{"select_static", "overflow", "input", "checker", ""} {
		evt := &callback.CardActionTriggerEvent{
			Event: &callback.CardActionTriggerRequest{
				Action: &callback.CallBackAction{Tag: tag, Value: map[string]any{"verb": "claim"}},
			},
		}
		resp, err := lws.handleCardAction(context.Background(), evt)
		if err != nil {
			t.Errorf("tag=%q err: %v", tag, err)
		}
		if resp != nil {
			t.Errorf("tag=%q should produce nil response; got %+v", tag, resp)
		}
	}
	if called {
		t.Error("non-button tags must not invoke the action handler")
	}
}

func TestLarkWS_HandleCardAction_NilCallback_NoPanic(t *testing.T) {
	lws := &LarkWSClient{onCardAction: nil}
	evt := &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_x"},
			Action:   &callback.CallBackAction{Tag: "button", Value: map[string]any{}},
		},
	}
	resp, err := lws.handleCardAction(context.Background(), evt)
	if err != nil {
		t.Errorf("nil onCardAction should be silently skipped; got %v", err)
	}
	if resp != nil {
		t.Errorf("expected nil response; got %+v", resp)
	}
}

func TestLarkWS_HandleCardAction_NilEvent_NoPanic(t *testing.T) {
	lws := &LarkWSClient{
		onCardAction: func(_ context.Context, _ string, _ map[string]any) (string, string) {
			t.Error("callback must not fire when event is nil")
			return "", ""
		},
	}
	if _, err := lws.handleCardAction(context.Background(), nil); err != nil {
		t.Errorf("nil event should be silently skipped; got %v", err)
	}
}

func TestLarkWS_HandleCardAction_NilInnerEvent_NoPanic(t *testing.T) {
	lws := &LarkWSClient{
		onCardAction: func(_ context.Context, _ string, _ map[string]any) (string, string) {
			t.Error("callback must not fire when inner event is nil")
			return "", ""
		},
	}
	evt := &callback.CardActionTriggerEvent{Event: nil}
	if _, err := lws.handleCardAction(context.Background(), evt); err != nil {
		t.Errorf("nil inner event should be silently skipped; got %v", err)
	}
}

func TestLarkWS_HandleCardAction_NilAction_DoesNotInvokeHandler(t *testing.T) {
	// event.Action == nil hits the same "tag != button" early-return.
	called := false
	lws := &LarkWSClient{
		onCardAction: func(_ context.Context, _ string, _ map[string]any) (string, string) {
			called = true
			return "", ""
		},
	}
	evt := &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{Operator: &callback.Operator{OpenID: "ou_x"}, Action: nil},
	}
	resp, err := lws.handleCardAction(context.Background(), evt)
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if resp != nil {
		t.Errorf("expected nil response; got %+v", resp)
	}
	if called {
		t.Error("nil Action must short-circuit before invoking the handler")
	}
}

func TestLarkWS_HandleCardAction_MissingOperator_HandlerSeesEmptyOpenID(t *testing.T) {
	// Operator omitted (no clicker identity surfaced); the handler still
	// runs — downstream code maps empty openID to a "user not linked"
	// toast rather than dropping the click silently.
	var gotOpenID string
	called := false
	lws := &LarkWSClient{
		onCardAction: func(_ context.Context, openID string, _ map[string]any) (string, string) {
			called = true
			gotOpenID = openID
			return "error", "missing open_id"
		},
	}
	evt := &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: nil,
			Action:   &callback.CallBackAction{Tag: "button", Value: map[string]any{"verb": "claim"}},
		},
	}
	resp, err := lws.handleCardAction(context.Background(), evt)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Fatal("handler must still run when operator is missing")
	}
	if gotOpenID != "" {
		t.Errorf("expected empty openID; got %q", gotOpenID)
	}
	if resp == nil || resp.Toast.Type != "error" {
		t.Errorf("response should propagate handler toast; got %+v", resp)
	}
}
