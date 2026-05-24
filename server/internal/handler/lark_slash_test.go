package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// ── Pure parser ─────────────────────────────────────────────────────────

func TestParseLarkSlashCommand(t *testing.T) {
	cases := []struct {
		in   string
		want larkSlashCommand
	}{
		// Exact matches across the trio.
		{"/help", larkSlashHelp},
		{"/status", larkSlashStatus},
		{"/whoami", larkSlashWhoami},

		// Surrounding whitespace and trailing args don't disqualify a
		// match — slash commands ignore args, but the bot should still
		// recognise them so users don't think the command is wrong.
		{"  /help  ", larkSlashHelp},
		{"/help me with this", larkSlashHelp},
		{"/status\t", larkSlashStatus},
		{"/whoami extra", larkSlashWhoami},

		// Word-boundaried: prefix match is intentional non-match.
		// "/helpme" is a different command (which we don't have), not
		// "/help" with an arg attached.
		{"/helpme", larkSlashNone},
		{"/statuscheck", larkSlashNone},

		// §11 invariant 4: no NLU. Aliases must not parse, even in CN.
		{"help", larkSlashNone},
		{"帮助", larkSlashNone},
		{"我是谁", larkSlashNone},

		// Non-slash text falls through.
		{"", larkSlashNone},
		{"hello", larkSlashNone},
		{"@bot 创建任务 fix bug", larkSlashNone},
	}
	for _, c := range cases {
		got := parseLarkSlashCommand(c.in)
		if got != c.want {
			t.Errorf("parseLarkSlashCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ── Integration: fake Lark API capturing both reply and send paths ─────

type larkFakeServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  []larkCapturedReply // POST /im/v1/messages/{id}/reply
	sends    []larkCapturedSend  // POST /im/v1/messages?receive_id_type=...
}

type larkCapturedReply struct {
	MessageID string
	Text      string
}

type larkCapturedSend struct {
	ReceiveIDType string
	ReceiveID     string
	Text          string
	MsgType       string
}

// installFakeLarkSlashServer wires a capturing fake API into
// testHandler.LarkThread for the duration of t. Both ReplyToMessage
// and SendTextMessage paths are captured separately so a test can
// assert "in-channel reply went here AND a DM went there" without
// inferring from a single mixed slice.
func installFakeLarkSlashServer(t *testing.T) *larkFakeServer {
	t.Helper()
	f := &larkFakeServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tenant_access_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tk", "expire": 7200,
			})
			return

		case strings.HasSuffix(r.URL.Path, "/reply"):
			// /im/v1/messages/{message_id}/reply
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/reply"), "/")
			messageID := parts[len(parts)-1]
			var p struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			text := extractTextField(p.Content)
			f.mu.Lock()
			f.replies = append(f.replies, larkCapturedReply{MessageID: messageID, Text: text})
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
			return

		case r.URL.Path == "/im/v1/messages":
			// Send path: receive_id_type from the query string.
			var p struct {
				ReceiveID string `json:"receive_id"`
				MsgType   string `json:"msg_type"`
				Content   string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			text := extractTextField(p.Content)
			f.mu.Lock()
			f.sends = append(f.sends, larkCapturedSend{
				ReceiveIDType: r.URL.Query().Get("receive_id_type"),
				ReceiveID:     p.ReceiveID,
				MsgType:       p.MsgType,
				Text:          text,
			})
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
			return
		}
		// Default success — anything we don't model.
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))

	cfg := service.LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "verify_test_token", EncryptKey: "encrypt_test_key"}
	client := service.NewLarkClient(cfg)
	service.SetAPIBaseForTest(client, f.srv.URL)
	prev := testHandler.LarkThread
	testHandler.LarkThread = service.NewLarkThreadService(testHandler.Queries, testPool, testHandler.Bus, client)
	t.Cleanup(func() {
		testHandler.LarkThread = prev
		f.srv.Close()
	})
	return f
}

func (f *larkFakeServer) Replies() []larkCapturedReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]larkCapturedReply, len(f.replies))
	copy(out, f.replies)
	return out
}

func (f *larkFakeServer) Sends() []larkCapturedSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]larkCapturedSend, len(f.sends))
	copy(out, f.sends)
	return out
}

// extractTextField unwraps Lark's `{"text":"..."}` content envelope so
// the test can assert on the human-readable body.
func extractTextField(content string) string {
	if content == "" {
		return ""
	}
	var inner struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &inner); err != nil {
		return content
	}
	return inner.Text
}

// buildLarkV2SlashMessage is like buildLarkV2MessageEvent but with a
// chat_type field — slash dispatch behaviour branches on group/p2p.
func buildLarkV2SlashMessage(senderOpenID, chatID, chatType, messageID, text string) []byte {
	body, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_type": "im.message.receive_v1",
			"token":      "verify_test_token",
		},
		"event": map[string]any{
			"sender": map[string]any{
				"sender_type": "user",
				"sender_id":   map[string]any{"open_id": senderOpenID},
			},
			"message": map[string]any{
				"message_id":   messageID,
				"chat_id":      chatID,
				"chat_type":    chatType,
				"message_type": "text",
				"content":      `{"text":"` + text + `"}`,
				"create_time":  "1747555200000",
			},
		},
	})
	return body
}

func postLarkWebhook(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	return rr
}

// ── /help ───────────────────────────────────────────────────────────────

func TestLarkSlash_Help_RepliesInBoth(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := installFakeLarkSlashServer(t)

	// /help should respond regardless of binding / linkage state. Run
	// in a group chat where neither is set up, then again in a DM —
	// both must produce a reply.
	postLarkWebhook(t, buildLarkV2SlashMessage("ou_help_group", "oc_help_group", "group", "om_help_group", "/help"))
	postLarkWebhook(t, buildLarkV2SlashMessage("ou_help_dm", "oc_help_dm", "p2p", "om_help_dm", "/help"))

	replies := fake.Replies()
	if len(replies) != 2 {
		t.Fatalf("expected 2 replies (group + dm), got %d: %+v", len(replies), replies)
	}
	for _, r := range replies {
		if !strings.Contains(r.Text, "Multica bot 指令") {
			t.Errorf("help reply missing header: %q", r.Text)
		}
		if !strings.Contains(r.Text, "创建任务") {
			t.Errorf("help reply missing 创建任务 verb: %q", r.Text)
		}
		if !strings.Contains(r.Text, "/whoami") {
			t.Errorf("help reply missing /whoami listing: %q", r.Text)
		}
	}
}

// ── /status ─────────────────────────────────────────────────────────────

func TestLarkSlash_Status_BoundChatShowsWorkspaceAndEvents(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := installFakeLarkSlashServer(t)

	const chatID = "oc_status_bound"
	seedLarkChatBinding(t, chatID)
	// Add an event to the binding so we can assert it surfaces.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE lark_workspace_binding SET enabled_events = ARRAY['issue:created']::text[] WHERE workspace_id = $1`,
		testWorkspaceID); err != nil {
		t.Fatalf("update events: %v", err)
	}

	postLarkWebhook(t, buildLarkV2SlashMessage("ou_status_user", chatID, "group", "om_status_msg", "/status"))

	replies := fake.Replies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	body := replies[0].Text
	if !strings.Contains(body, "Bot:") {
		t.Errorf("status missing Bot line: %q", body)
	}
	if !strings.Contains(body, "Workspace:") {
		t.Errorf("status missing Workspace line: %q", body)
	}
	if !strings.Contains(body, "issue:created") {
		t.Errorf("status missing enabled event: %q", body)
	}
	// Privacy: no user-level prefs language should leak into /status.
	if strings.Contains(body, "Linked") || strings.Contains(body, "open_id") {
		t.Errorf("status leaked per-user state: %q", body)
	}
}

func TestLarkSlash_Status_UnboundChatSurfacesMissingBinding(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := installFakeLarkSlashServer(t)

	// No binding seeded — /status should explain rather than crash or
	// silently no-op. Discoverability beats stoicism here.
	postLarkWebhook(t, buildLarkV2SlashMessage("ou_status_unbound", "oc_status_unbound", "group", "om_status_unbound_msg", "/status"))

	replies := fake.Replies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "not bound") {
		t.Errorf("expected 'not bound' notice, got %q", replies[0].Text)
	}
}

// ── /whoami ─────────────────────────────────────────────────────────────

func TestLarkSlash_Whoami_GroupReplyIsNeutralAndDMHasDetails(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := installFakeLarkSlashServer(t)

	const openID = "ou_whoami_linked_in_group"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	postLarkWebhook(t, buildLarkV2SlashMessage(openID, "oc_whoami_group", "group", "om_whoami_group_msg", "/whoami"))

	// In-channel reply must be the constant neutral text — never leak
	// linkage state to bystanders.
	replies := fake.Replies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 in-channel reply, got %d", len(replies))
	}
	if replies[0].Text != larkWhoamiGroupNeutral {
		t.Errorf("group reply must be exact neutral text\n  got: %q\n  want: %q", replies[0].Text, larkWhoamiGroupNeutral)
	}

	// DM fan-out should carry the actual binding details, targeted
	// at the sender's open_id.
	sends := fake.Sends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 DM send, got %d: %+v", len(sends), sends)
	}
	if sends[0].ReceiveIDType != "open_id" {
		t.Errorf("DM should use receive_id_type=open_id, got %q", sends[0].ReceiveIDType)
	}
	if sends[0].ReceiveID != openID {
		t.Errorf("DM target must be sender open_id %q, got %q", openID, sends[0].ReceiveID)
	}
	if !strings.Contains(sends[0].Text, "已绑定") {
		t.Errorf("DM body missing 已绑定 status: %q", sends[0].Text)
	}
}

func TestLarkSlash_Whoami_UnlinkedUserInGroupGetsNeutralReplyOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := installFakeLarkSlashServer(t)

	// No lark_user_link seeded. The user must see the SAME neutral
	// reply as a linked user — that's the §14.1.2 privacy contract.
	// And NO DM should be sent (we have nowhere to send it).
	postLarkWebhook(t, buildLarkV2SlashMessage("ou_whoami_unlinked", "oc_whoami_unlinked_group", "group", "om_whoami_unlinked_msg", "/whoami"))

	replies := fake.Replies()
	if len(replies) != 1 || replies[0].Text != larkWhoamiGroupNeutral {
		t.Fatalf("unlinked user must get the exact neutral reply, got %+v", replies)
	}
	sends := fake.Sends()
	if len(sends) != 0 {
		t.Errorf("no DM should be sent for unlinked user, got %+v", sends)
	}
}

func TestLarkSlash_Whoami_DMRepliesInPlaceWithDetails(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := installFakeLarkSlashServer(t)

	const openID = "ou_whoami_linked_in_dm"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	postLarkWebhook(t, buildLarkV2SlashMessage(openID, "oc_whoami_dm_chat", "p2p", "om_whoami_dm_msg", "/whoami"))

	// In a DM, the reply itself can carry the details (no bystanders).
	replies := fake.Replies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply, got %d", len(replies))
	}
	if !strings.Contains(replies[0].Text, "已绑定") {
		t.Errorf("DM-chat reply missing 已绑定 status: %q", replies[0].Text)
	}
	// No extra DM fan-out — the in-channel reply IS the DM.
	if got := len(fake.Sends()); got != 0 {
		t.Errorf("DM-chat should not fan out an extra send, got %d", got)
	}
}
