package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
)

// withLarkWebhookEnv flips on the env vars the webhook handler reads.
// Kept separate from withLarkEnv (settings tests) because the values
// participate in signature computation in this file.
func withLarkWebhookEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LARK_APP_ID", "cli_test_id")
	t.Setenv("LARK_APP_SECRET", "cli_test_secret")
	t.Setenv("LARK_VERIFICATION_TOKEN", "verify_test_token")
	t.Setenv("LARK_ENCRYPT_KEY", "encrypt_test_key")
}

// signLarkBody mirrors the production header-based signature scheme. We
// deliberately keep the test impl alongside the production one — if the
// scheme changes both move together and we notice.
func signLarkBody(ts, nonce, encryptKey string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(ts))
	h.Write([]byte(nonce))
	h.Write([]byte(encryptKey))
	h.Write(body)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// signLarkRequest attaches a valid X-Lark-Signature triple to req using
// the test encrypt key. Lark v2 event subscriptions don't carry a flat
// `token` field that the body-fallback path can match against, so any
// test that POSTs a v2 envelope must sign the request.
func signLarkRequest(req *http.Request, body []byte) {
	const ts = "1700000000"
	const nonce = "n_test"
	sig := signLarkBody(ts, nonce, "encrypt_test_key", body)
	req.Header.Set("X-Lark-Signature", sig)
	req.Header.Set("X-Lark-Request-Timestamp", ts)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
}

func TestLarkWebhook_URLVerificationChallengeEchoesBackViaTokenFallback(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}

	body, _ := json.Marshal(map[string]string{
		"type":      "url_verification",
		"token":     "verify_test_token",
		"challenge": "challenge_xyz",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["challenge"] != "challenge_xyz" {
		t.Fatalf("expected challenge echoed, got %v", out)
	}
}

func TestLarkWebhook_SignedRequestPasses(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}

	body, _ := json.Marshal(map[string]any{
		"type":      "url_verification",
		"token":     "irrelevant_under_signature",
		"challenge": "abc",
	})
	const ts = "1715900000"
	const nonce = "n1"
	sig := signLarkBody(ts, nonce, "encrypt_test_key", body)

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	req.Header.Set("X-Lark-Request-Timestamp", ts)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
	req.Header.Set("X-Lark-Signature", sig)
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
}

func TestLarkWebhook_TamperedSignatureFails(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}

	body, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": "abc"})
	sig := signLarkBody("1715900000", "n", "encrypt_test_key", body)
	tampered := sig[:len(sig)-1] + "A"

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	req.Header.Set("X-Lark-Request-Timestamp", "1715900000")
	req.Header.Set("X-Lark-Request-Nonce", "n")
	req.Header.Set("X-Lark-Signature", tampered)
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered sig, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLarkWebhook_WrongTokenFallbackFails(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}
	body, _ := json.Marshal(map[string]string{
		"type":      "url_verification",
		"token":     "not_the_real_token",
		"challenge": "abc",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLarkWebhook_NoSecretsRefusesEverything(t *testing.T) {
	t.Setenv("LARK_ENCRYPT_KEY", "")
	t.Setenv("LARK_VERIFICATION_TOKEN", "")
	h := &Handler{}

	body, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": "abc"})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when env unset, got %d", rr.Code)
	}
}

func TestLarkWebhook_UnknownActionTagAcked(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}
	body, _ := json.Marshal(map[string]any{
		"token":   "verify_test_token",
		"open_id": "ou_x",
		"action": map[string]any{
			"tag":   "select_static",
			"value": map[string]any{"verb": "anything"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected silent 200 ack, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "{}") {
		t.Fatalf("expected empty body, got %s", rr.Body.String())
	}
}

func TestLarkWebhook_UnlinkedClickerGetsLinkPrompt(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)

	body, _ := json.Marshal(map[string]any{
		"token":   "verify_test_token",
		"open_id": "ou_unknown_user",
		"action": map[string]any{
			"tag":   "button",
			"value": map[string]any{"verb": "claim", "issue_id": "00000000-0000-0000-0000-000000000000"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	// Toast type=warning + "Link your Lark account" prompt is the contract.
	bodyStr := rr.Body.String()
	if !strings.Contains(bodyStr, `"type":"warning"`) ||
		!strings.Contains(bodyStr, "Link your Lark account") {
		t.Fatalf("expected link-prompt toast, got %s", bodyStr)
	}
}

func TestLarkWebhook_ClaimVerb_AssignsIssueToClicker(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)

	// Seed: link this test's multica user to a Lark open_id, and create
	// an unassigned issue in the test workspace.
	const openID = "ou_claim_happy_path"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'Claim me', '', 'todo', 'medium', 'member', $2, 9001)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	body, _ := json.Marshal(map[string]any{
		"token":   "verify_test_token",
		"open_id": openID,
		"action": map[string]any{
			"tag":   "button",
			"value": map[string]any{"verb": "claim", "issue_id": issueID},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"type":"success"`) {
		t.Fatalf("expected success toast, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Verify the DB now has the issue assigned to the test user.
	var assigneeType pgtype.Text
	var assigneeID pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`SELECT assignee_type, assignee_id FROM issue WHERE id = $1`, issueID,
	).Scan(&assigneeType, &assigneeID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !assigneeType.Valid || assigneeType.String != "member" {
		t.Fatalf("assignee_type = %+v, want member", assigneeType)
	}
	if !assigneeID.Valid {
		t.Fatalf("assignee_id not set")
	}
}

func TestLarkWebhook_MarkDoneVerb_FlipsStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)

	const openID = "ou_mark_done_happy_path"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'Finish me', '', 'in_progress', 'medium', 'member', $2, 9002)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	body, _ := json.Marshal(map[string]any{
		"token":   "verify_test_token",
		"open_id": openID,
		"action": map[string]any{
			"tag":   "button",
			"value": map[string]any{"verb": "mark_done", "issue_id": issueID},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"type":"success"`) {
		t.Fatalf("expected success, got %d body=%s", rr.Code, rr.Body.String())
	}

	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
}

// ── P4: @bot message-event dispatch ─────────────────────────────────────

// installFakeLarkThreadService points testHandler.LarkThread at a Lark
// API stub so reply calls (ReplyToMessage / ListThreadMessages) hit a
// local httptest server instead of the real open.feishu.cn endpoint.
// The stub always returns success (code=0). Returns a teardown that
// restores the original wiring; callers register it via t.Cleanup.
func installFakeLarkThreadService(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tenant_access_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tk", "expire": 7200,
			})
		case strings.Contains(r.URL.Path, "/im/v1/messages") && r.Method == http.MethodGet:
			// Empty thread list — no transcript to surface in issue body.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "data": map[string]any{"items": []any{}},
			})
		case strings.Contains(r.URL.Path, "/reply"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		}
	}))
	cfg := service.LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "verify_test_token", EncryptKey: "encrypt_test_key"}
	client := service.NewLarkClient(cfg)
	service.SetAPIBaseForTest(client, srv.URL)

	prev := testHandler.LarkThread
	testHandler.LarkThread = service.NewLarkThreadService(testHandler.Queries, testPool, testHandler.Bus, client)
	t.Cleanup(func() {
		testHandler.LarkThread = prev
		srv.Close()
	})
}

// seedLarkChatBinding inserts a lark_workspace_binding for the test
// workspace + given chat_id, registers a cleanup to roll it back.
func seedLarkChatBinding(t *testing.T, chatID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_workspace_binding (workspace_id, chat_id) VALUES ($1, $2)
		 ON CONFLICT (workspace_id) DO UPDATE SET chat_id = EXCLUDED.chat_id`,
		testWorkspaceID, chatID); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM lark_workspace_binding WHERE workspace_id = $1`, testWorkspaceID)
	})
}

// buildLarkV2MessageEvent wraps a message into Lark's v2 event
// subscription envelope. Only the fields the dispatcher reads are
// populated — everything else stays defaulted.
func buildLarkV2MessageEvent(senderOpenID, chatID, threadID, messageID, text string) []byte {
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
				"thread_id":    threadID,
				"message_type": "text",
				"content":      `{"text":"` + text + `"}`,
				"create_time":  "1747555200000",
			},
		},
	})
	return body
}

func TestLarkWebhook_CreateIssueVerb_CreatesIssueAndLinkRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	installFakeLarkThreadService(t)

	const openID = "ou_create_issue_happy_path"
	const chatID = "oc_p4_create_happy"
	const triggerMsgID = "om_trigger_create_happy"
	seedLarkChatBinding(t, chatID)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	body := buildLarkV2MessageEvent(openID, chatID, "", triggerMsgID,
		"@_user_1 创建任务 fix the flaky CI on Tuesdays")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}

	// Verify: issue was created with the resolved title, and the
	// lark_issue_link row points back at the trigger message id (we
	// passed no thread_id, so the anchor IS the trigger message).
	var issueID string
	var title string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id, title FROM issue
		 WHERE workspace_id = $1 AND creator_id::text = $2
		 ORDER BY created_at DESC LIMIT 1`,
		testWorkspaceID, testUserID).Scan(&issueID, &title); err != nil {
		t.Fatalf("read back issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})
	if title != "fix the flaky CI on Tuesdays" {
		t.Fatalf("title = %q", title)
	}

	var linkChatID, linkRoot string
	if err := testPool.QueryRow(context.Background(),
		`SELECT chat_id, root_message_id FROM lark_issue_link WHERE issue_id = $1`,
		issueID).Scan(&linkChatID, &linkRoot); err != nil {
		t.Fatalf("read back link: %v", err)
	}
	if linkChatID != chatID {
		t.Fatalf("link.chat_id = %q, want %q", linkChatID, chatID)
	}
	if linkRoot != triggerMsgID {
		t.Fatalf("link.root_message_id = %q, want %q", linkRoot, triggerMsgID)
	}
}

func TestLarkWebhook_CreateIssueVerb_UnlinkedUserGetsBindHint(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	installFakeLarkThreadService(t)

	const chatID = "oc_p4_unlinked"
	seedLarkChatBinding(t, chatID)

	body := buildLarkV2MessageEvent("ou_someone_not_linked", chatID, "", "om_trigger",
		"@_user_1 创建任务 do the thing")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	// No issue should have been created — the dispatcher short-circuits
	// before CreateIssueFromThread when the sender isn't linked.
	var count int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue WHERE workspace_id = $1 AND title = $2`,
		testWorkspaceID, "do the thing").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no issue created, got %d", count)
	}
}

func TestLarkWebhook_UnknownVerb_SilentAck(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	installFakeLarkThreadService(t)

	const chatID = "oc_p4_unknown_verb"
	seedLarkChatBinding(t, chatID)
	const openID = "ou_unknown_verb_user"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	body := buildLarkV2MessageEvent(openID, chatID, "", "om_irrelevant",
		"@_user_1 just chatting, no verb here")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	// No issue, no link — the dispatcher returned early on
	// ParseLarkBotVerb == LarkVerbNone.
	var count int
	testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM lark_issue_link l
		 JOIN issue i ON i.id = l.issue_id
		 WHERE i.workspace_id = $1 AND l.chat_id = $2`,
		testWorkspaceID, chatID).Scan(&count)
	if count != 0 {
		t.Fatalf("expected no link row, got %d", count)
	}
}

func TestLarkWebhook_UnboundChat_SilentAck(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	installFakeLarkThreadService(t)

	// No binding inserted — bot is in a chat that isn't bound to any
	// workspace. The dispatcher must silently drop rather than guess
	// which workspace the issue belongs to.
	body := buildLarkV2MessageEvent("ou_anyone", "oc_p4_unbound_chat", "", "om_a",
		"@_user_1 创建任务 should be ignored")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}

	var count int
	testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue WHERE title = $1`, "should be ignored").Scan(&count)
	if count != 0 {
		t.Fatalf("issue created for unbound chat (count=%d)", count)
	}
}

// ── P5: outbound mirror (agent comment → Lark thread) ──────────────────

// TestLarkThread_MirrorAgentComment_RepliesIntoThread proves that
// MirrorAgentCommentToThread:
//   - resolves the lark_issue_link by issue_id
//   - posts a "[multica] <author>: <content>" reply via Lark's reply API
//   - aims at the link.root_message_id (the thread anchor)
//
// This is the seam the P5 bus listener uses to bridge agent
// clarification comments back into the originating Lark thread.
func TestLarkThread_MirrorAgentComment_RepliesIntoThread(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)

	var capturedPath string
	var capturedPayload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tenant_access_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tk", "expire": 7200,
			})
		case strings.Contains(r.URL.Path, "/reply"):
			capturedPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&capturedPayload)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
		}
	}))
	t.Cleanup(srv.Close)

	cfg := service.LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "verify_test_token", EncryptKey: "encrypt_test_key"}
	client := service.NewLarkClient(cfg)
	service.SetAPIBaseForTest(client, srv.URL)
	prev := testHandler.LarkThread
	testHandler.LarkThread = service.NewLarkThreadService(testHandler.Queries, testPool, testHandler.Bus, client)
	t.Cleanup(func() { testHandler.LarkThread = prev })

	// Seed an issue + a lark_issue_link pointing at a known root message.
	const chatID = "oc_p5_mirror"
	const rootMsgID = "om_p5_root_for_mirror"
	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'P5 mirror seed', '', 'todo', 'medium', 'member', $2, 9101)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_issue_link (issue_id, chat_id, root_message_id) VALUES ($1, $2, $3)`,
		issueID, chatID, rootMsgID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	var issueUUID pgtype.UUID
	if err := issueUUID.Scan(issueID); err != nil {
		t.Fatalf("scan issue uuid: %v", err)
	}
	testHandler.LarkThread.MirrorAgentCommentToThread(context.Background(), issueUUID, "CodexBot",
		"Should this column be UUID or ULID?")

	if !strings.HasSuffix(capturedPath, "/im/v1/messages/"+rootMsgID+"/reply") {
		t.Fatalf("reply went to %q, want anchor on root message id", capturedPath)
	}
	contentStr, _ := capturedPayload["content"].(string)
	var content struct{ Text string }
	if err := json.Unmarshal([]byte(contentStr), &content); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if !strings.HasPrefix(content.Text, "[multica] CodexBot:") {
		t.Fatalf("body missing prefix/author: %q", content.Text)
	}
	if !strings.Contains(content.Text, "Should this column be UUID or ULID?") {
		t.Fatalf("body missing question: %q", content.Text)
	}
}

// TestLarkThread_MirrorAgentComment_NoLinkRowSilentNoop proves that an
// issue with no lark_issue_link row produces no Lark API call — the
// mirror is opt-in via the link table; issues born inside multica must
// not leak into a chat that doesn't know about them.
func TestLarkThread_MirrorAgentComment_NoLinkRowSilentNoop(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reply") {
			calls++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
	}))
	t.Cleanup(srv.Close)

	cfg := service.LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "verify_test_token", EncryptKey: "encrypt_test_key"}
	client := service.NewLarkClient(cfg)
	service.SetAPIBaseForTest(client, srv.URL)
	prev := testHandler.LarkThread
	testHandler.LarkThread = service.NewLarkThreadService(testHandler.Queries, testPool, testHandler.Bus, client)
	t.Cleanup(func() { testHandler.LarkThread = prev })

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'P5 mirror no-link', '', 'todo', 'medium', 'member', $2, 9102)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	var issueUUID pgtype.UUID
	if err := issueUUID.Scan(issueID); err != nil {
		t.Fatalf("scan issue uuid: %v", err)
	}
	testHandler.LarkThread.MirrorAgentCommentToThread(context.Background(), issueUUID, "bot", "hello")

	if calls != 0 {
		t.Fatalf("expected no reply calls for unlinked issue, got %d", calls)
	}
}

// silence linter — pgtype is imported by the file via earlier tests.
var _ = pgtype.Text{}
