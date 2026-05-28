package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Cross-direction integration tests covering scenarios 1 and 4 from
// LARK_INTEGRATION_TEST.md.
//
// These tests exercise multiple real layers at once:
//
//   - Scenario 1: HTTP webhook → LarkThread service → fake Lark API
//     (verifies the confirmation reply payload that lands back in the
//     originating thread)
//   - Scenario 4: real issue row in test DB → fetchLinkedDocsForIssue →
//     fake Lark docs API → service.LinkedDoc slice with shape compatible
//     with the daemon-side daemon.LinkedDoc (verified by the matching
//     prompt-builder tests in internal/daemon/prompt_test.go)
//
// The fake Lark API records every captured request so assertions can
// inspect the actual JSON body, URL paths, and call counts — the half
// of the integration that unit tests with mocked clients cannot cover.

// ── Fake Lark API plumbing ─────────────────────────────────────────────

// fakeLarkAPI captures every Lark API request the system under test
// sends. Tests register per-path handlers via Handle; everything else
// falls through to a default success response so unrelated calls (token
// refresh, GET thread history) don't break the test.
type fakeLarkAPI struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []fakeLarkRequest
	handlers map[string]http.HandlerFunc
}

type fakeLarkRequest struct {
	Path   string
	Method string
	Body   []byte
}

func newFakeLarkAPI(t *testing.T) *fakeLarkAPI {
	t.Helper()
	f := &fakeLarkAPI{handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, fakeLarkRequest{Path: r.URL.Path, Method: r.Method, Body: body})
		// Look up the longest registered prefix that matches. Lark routes
		// share common ancestors (every endpoint lives under /open-apis/)
		// so the most specific registration wins.
		var bestPath string
		var bestHandler http.HandlerFunc
		for prefix, h := range f.handlers {
			if strings.Contains(r.URL.Path, prefix) && len(prefix) > len(bestPath) {
				bestPath, bestHandler = prefix, h
			}
		}
		f.mu.Unlock()
		if bestHandler != nil {
			// Replay the body so handlers can decode it.
			r.Body = io.NopCloser(bytes.NewReader(body))
			bestHandler(w, r)
			return
		}
		// Default: token endpoint returns a fresh tenant token; everything
		// else returns code=0 so nothing accidentally fails on a path the
		// test hasn't pinned.
		if strings.Contains(r.URL.Path, "/tenant_access_token") {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLarkAPI) URL() string { return f.srv.URL }

// Handle registers a handler for any request whose path contains the
// given substring. Most specific (longest) match wins at request time.
func (f *fakeLarkAPI) Handle(pathSubstr string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[pathSubstr] = h
}

// CapturedRequests returns a snapshot of every request seen so far.
func (f *fakeLarkAPI) CapturedRequests() []fakeLarkRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeLarkRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// FindRequest returns the first captured request whose path contains
// pathSubstr, or nil if none matches.
func (f *fakeLarkAPI) FindRequest(pathSubstr string) *fakeLarkRequest {
	for _, req := range f.CapturedRequests() {
		if strings.Contains(req.Path, pathSubstr) {
			return &req
		}
	}
	return nil
}

// FindRequestByMethod is FindRequest narrowed to a specific HTTP method —
// needed when Lark routes overload one path (e.g. /im/v1/messages serves
// GET history alongside POST send/reply) and the test needs the write.
func (f *fakeLarkAPI) FindRequestByMethod(method, pathSubstr string) *fakeLarkRequest {
	for _, req := range f.CapturedRequests() {
		if req.Method == method && strings.Contains(req.Path, pathSubstr) {
			return &req
		}
	}
	return nil
}

// installFakeLarkDocs swaps testHandler.LarkDocs for a service backed
// by a client pointing at fake. Returns the fake so the test can pin
// per-endpoint behavior. The previous LarkDocs is restored on cleanup.
func installFakeLarkDocs(t *testing.T, fake *fakeLarkAPI) {
	t.Helper()
	cfg := service.LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "verify_test_token", EncryptKey: "encrypt_test_key"}
	client := service.NewLarkClient(cfg)
	service.SetAPIBaseForTest(client, fake.URL())
	prev := testHandler.LarkDocs
	testHandler.LarkDocs = service.NewLarkDocs(client)
	t.Cleanup(func() { testHandler.LarkDocs = prev })
}

// ── Scenario 4: LinkedDocs end-to-end ──────────────────────────────────

// TestIntegration_LinkedDocs_DocxFetchEndToEnd drives the full claim-time
// expansion path with a real issue row, a real fetchLinkedDocsForIssue
// call, and a fake Lark API serving the docx raw_content endpoint. The
// assertion is that LinkedDoc.Content comes back populated with the
// fake's response body and the URL is preserved verbatim — this is the
// contract the daemon's prompt builder relies on (see
// internal/daemon/prompt_test.go::TestBuildPromptLinkedDocs_DefaultBranch).
func TestIntegration_LinkedDocs_DocxFetchEndToEnd(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}

	const docxBody = "Section A: the spec body the agent must read"
	fake := newFakeLarkAPI(t)
	fake.Handle("/docx/v1/documents/AbcDoc/raw_content", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"content": docxBody},
		})
	})
	installFakeLarkDocs(t, fake)

	// Seed an issue whose description references the docx URL. The
	// fetcher scans description + comments, so this is the minimum.
	const docURL = "https://acme.feishu.cn/docx/AbcDoc"
	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'docs e2e', $2, 'todo', 'medium', 'member', $3, 9501)
		RETURNING id
	`, testWorkspaceID, "See spec at "+docURL, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	docs := testHandler.fetchLinkedDocsForIssue(context.Background(), issue)
	if len(docs) != 1 {
		t.Fatalf("expected 1 linked doc, got %d (%+v)", len(docs), docs)
	}
	got := docs[0]
	if got.URL != docURL {
		t.Errorf("LinkedDoc.URL = %q, want %q", got.URL, docURL)
	}
	if got.Error != "" {
		t.Errorf("LinkedDoc.Error = %q, want empty (success path)", got.Error)
	}
	if got.Content != docxBody {
		t.Errorf("LinkedDoc.Content = %q, want %q", got.Content, docxBody)
	}

	// The fake API must have actually seen the raw_content fetch — guards
	// against the "configured but never called" silent-failure mode where
	// the fetcher returns nil because the integration looked disabled.
	if fake.FindRequest("/docx/v1/documents/AbcDoc/raw_content") == nil {
		t.Errorf("expected fake Lark API to see raw_content fetch, got requests:\n%+v", fake.CapturedRequests())
	}
	if fake.FindRequest("/tenant_access_token") == nil {
		t.Errorf("expected tenant_access_token fetch before docx call")
	}
}

// TestIntegration_LinkedDocs_WikiResolvesToDocx exercises the wiki
// branch: the fetcher first resolves the wiki node to its underlying
// docx obj_token, then fetches that docx's raw content. Both fake
// endpoints must be hit in order.
func TestIntegration_LinkedDocs_WikiResolvesToDocx(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}

	const wikiBody = "Wiki-backed docx body"
	fake := newFakeLarkAPI(t)
	fake.Handle("/wiki/v2/spaces/get_node", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"node": map[string]any{
					"obj_token": "ResolvedDocx",
					"obj_type":  "docx",
				},
			},
		})
	})
	fake.Handle("/docx/v1/documents/ResolvedDocx/raw_content", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"content": wikiBody},
		})
	})
	installFakeLarkDocs(t, fake)

	const wikiURL = "https://acme.feishu.cn/wiki/WikiTok"
	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'wiki e2e', $2, 'todo', 'medium', 'member', $3, 9502)
		RETURNING id
	`, testWorkspaceID, "Wiki ref: "+wikiURL, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	docs := testHandler.fetchLinkedDocsForIssue(context.Background(), issue)
	if len(docs) != 1 || docs[0].Content != wikiBody || docs[0].Error != "" {
		t.Fatalf("wiki fetch returned %+v, want one populated doc with body %q", docs, wikiBody)
	}

	// Both endpoints must be hit; the resolve must precede the raw fetch.
	reqs := fake.CapturedRequests()
	var resolveIdx, fetchIdx = -1, -1
	for i, r := range reqs {
		if strings.Contains(r.Path, "/wiki/v2/spaces/get_node") {
			resolveIdx = i
		}
		if strings.Contains(r.Path, "/docx/v1/documents/ResolvedDocx/raw_content") {
			fetchIdx = i
		}
	}
	if resolveIdx < 0 || fetchIdx < 0 {
		t.Errorf("missing wiki resolve or docx fetch in captured requests:\n%+v", reqs)
	}
	if resolveIdx > fetchIdx {
		t.Errorf("wiki resolve (idx=%d) must precede docx fetch (idx=%d)", resolveIdx, fetchIdx)
	}
}

// TestIntegration_LinkedDocs_MixedSuccessAndFailure_E2E pins the
// degradation behavior: when one doc fetch returns 403, one returns 404,
// and one succeeds, the resulting LinkedDoc slice has three entries with
// the correct Error vocabulary ("forbidden", "not_found", "") and order
// preserved by first appearance in the description.
//
// This is the integration counterpart to the unit-level mix test in
// internal/daemon/prompt_test.go::TestBuildPromptLinkedDocs_MixedSuccessAndFailure
// — that one verifies the prompt rendering; this one verifies the
// server-side fetcher feeds the right shape into the daemon.
func TestIntegration_LinkedDocs_MixedSuccessAndFailure_E2E(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}

	fake := newFakeLarkAPI(t)
	fake.Handle("/docx/v1/documents/OkDoc/raw_content", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"content": "ok body"},
		})
	})
	fake.Handle("/docx/v1/documents/ForbiddenDoc/raw_content", func(w http.ResponseWriter, r *http.Request) {
		// HTTP 403 → fetchErrorCode maps to "forbidden".
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 1254003, "msg": "no permission"})
	})
	fake.Handle("/docx/v1/documents/MissingDoc/raw_content", func(w http.ResponseWriter, r *http.Request) {
		// 1254404 is in isLarkNotFound; we deliberately avoid 1254100
		// (which is in isLarkForbidden) because fetchErrorCode checks
		// the code list before the HTTP status, so a 404+1254100 would
		// resolve to "forbidden", not "not_found". This is intentional
		// upstream — Lark codes carry more information than the
		// status — but the test must thread that needle.
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 1254404, "msg": "not found"})
	})
	installFakeLarkDocs(t, fake)

	desc := "First: https://acme.feishu.cn/docx/OkDoc\n" +
		"Then: https://acme.feishu.cn/docx/ForbiddenDoc\n" +
		"Last: https://acme.feishu.cn/docx/MissingDoc\n"

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'mixed e2e', $2, 'todo', 'medium', 'member', $3, 9503)
		RETURNING id
	`, testWorkspaceID, desc, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	docs := testHandler.fetchLinkedDocsForIssue(context.Background(), issue)
	if len(docs) != 3 {
		t.Fatalf("expected 3 linked docs, got %d (%+v)", len(docs), docs)
	}

	type want struct {
		url     string
		errCode string
		content string
	}
	expected := []want{
		{"https://acme.feishu.cn/docx/OkDoc", "", "ok body"},
		{"https://acme.feishu.cn/docx/ForbiddenDoc", "forbidden", ""},
		{"https://acme.feishu.cn/docx/MissingDoc", "not_found", ""},
	}
	for i, exp := range expected {
		if docs[i].URL != exp.url {
			t.Errorf("docs[%d].URL = %q, want %q", i, docs[i].URL, exp.url)
		}
		if docs[i].Error != exp.errCode {
			t.Errorf("docs[%d].Error = %q, want %q", i, docs[i].Error, exp.errCode)
		}
		if docs[i].Content != exp.content {
			t.Errorf("docs[%d].Content = %q, want %q", i, docs[i].Content, exp.content)
		}
	}
}

// TestIntegration_LinkedDocs_FromCommentBody verifies the fetcher also
// surfaces docx URLs that appear only in comments, not the description.
// This closes the loop from "user pastes spec link in a follow-up
// comment" → "agent prompt still sees the doc body".
func TestIntegration_LinkedDocs_FromCommentBody(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}

	const docxBody = "Body referenced only via a comment"
	fake := newFakeLarkAPI(t)
	fake.Handle("/docx/v1/documents/CommentDoc/raw_content", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"content": docxBody},
		})
	})
	installFakeLarkDocs(t, fake)

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'comment-doc e2e', 'no urls in description', 'todo', 'medium', 'member', $2, 9504)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO comment (issue_id, workspace_id, content, author_type, author_id)
		VALUES ($1, $2, $3, 'member', $4)
	`, issueID, testWorkspaceID,
		"See https://acme.feishu.cn/docx/CommentDoc for the latest spec.", testUserID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}

	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	docs := testHandler.fetchLinkedDocsForIssue(context.Background(), issue)
	if len(docs) != 1 || docs[0].Content != docxBody {
		t.Fatalf("expected single doc fetched via comment body, got %+v", docs)
	}
}

// TestIntegration_LinkedDocs_NilWhenLarkDocsUnconfigured pins the
// no-Lark fallback: when h.LarkDocs is nil (the unconfigured production
// deployment), fetchLinkedDocsForIssue returns nil — the claim response
// omits linked_docs entirely and the daemon prompt stays byte-identical
// to the pre-P3 era.
func TestIntegration_LinkedDocs_NilWhenLarkDocsUnconfigured(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	prev := testHandler.LarkDocs
	testHandler.LarkDocs = nil
	t.Cleanup(func() { testHandler.LarkDocs = prev })

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'unconfigured', 'https://acme.feishu.cn/docx/Whatever', 'todo', 'medium', 'member', $2, 9505)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("reload issue: %v", err)
	}
	if docs := testHandler.fetchLinkedDocsForIssue(context.Background(), issue); docs != nil {
		t.Fatalf("expected nil LinkedDocs with no LarkDocs configured, got %+v", docs)
	}
}

// ── Scenario 1 (subset): @bot 创建任务 webhook captures confirmation reply ─

// installFakeLarkThreadServiceCapturing mirrors installFakeLarkThreadService
// but also records every reply request so the test can inspect the
// confirmation card sent back into the thread.
func installFakeLarkThreadServiceCapturing(t *testing.T, fake *fakeLarkAPI) {
	t.Helper()
	cfg := service.LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "verify_test_token", EncryptKey: "encrypt_test_key"}
	client := service.NewLarkClient(cfg)
	service.SetAPIBaseForTest(client, fake.URL())
	prev := testHandler.LarkThread
	testHandler.LarkThread = service.NewLarkThreadService(testHandler.Queries, testPool, testHandler.Bus, client)
	t.Cleanup(func() { testHandler.LarkThread = prev })

	// Lark routes share the /im/v1/messages prefix for several distinct
	// operations: GET for thread history, POST for send-new, POST .../reply
	// for replies. The single handler branches on method so capture stays
	// correct regardless of which path the dispatcher takes.
	fake.Handle("/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "data": map[string]any{"items": []any{}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "data": map[string]any{"message_id": "om_synth_reply"},
		})
	})
}

// TestIntegration_LarkThread_CreateIssueWebhook_CapturesConfirmationReply
// drives scenario 1 from the webhook entry point through to the fake
// Lark API receiving the confirmation reply card. It verifies that:
//
//  1. The webhook signature path passes
//  2. An issue row is created with the title parsed from the @bot
//     command
//  3. A lark_issue_link row points back at the trigger message
//  4. The fake Lark API received a POST to .../messages/<trigger>/reply
//     (or POST .../messages for the new-thread case) carrying an
//     interactive card whose body mentions the new issue
//
// The "fake Lark received the call" half is what distinguishes this
// from the existing TestLarkWebhook_CreateIssueVerb_CreatesIssueAndLinkRow,
// which only asserts DB state and never inspects what went out over the
// wire.
func TestIntegration_LarkThread_CreateIssueWebhook_CapturesConfirmationReply(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)

	fake := newFakeLarkAPI(t)
	installFakeLarkThreadServiceCapturing(t, fake)

	const openID = "ou_e2e_create_issue"
	const chatID = "oc_e2e_create_issue"
	const triggerMsgID = "om_e2e_trigger"
	seedLarkChatBinding(t, chatID)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	const issueTitle = "ship the integration test harness"
	body := buildLarkV2MessageEvent(openID, chatID, "", triggerMsgID,
		"@_user_1 创建任务 "+issueTitle)
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("webhook status = %d body = %s", rr.Code, rr.Body.String())
	}

	// DB side: issue + lark_issue_link landed.
	var issueID, gotTitle string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id, title FROM issue
		 WHERE workspace_id = $1 AND creator_id::text = $2
		 ORDER BY created_at DESC LIMIT 1`,
		testWorkspaceID, testUserID).Scan(&issueID, &gotTitle); err != nil {
		t.Fatalf("read back issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})
	if gotTitle != issueTitle {
		t.Errorf("issue.title = %q, want %q", gotTitle, issueTitle)
	}
	var linkChat, linkRoot string
	if err := testPool.QueryRow(context.Background(),
		`SELECT chat_id, root_message_id FROM lark_issue_link WHERE issue_id = $1`,
		issueID).Scan(&linkChat, &linkRoot); err != nil {
		t.Fatalf("read back link: %v", err)
	}
	if linkChat != chatID {
		t.Errorf("link.chat_id = %q, want %q", linkChat, chatID)
	}
	if linkRoot != triggerMsgID {
		t.Errorf("link.root_message_id = %q, want %q", linkRoot, triggerMsgID)
	}

	// Wire side: the fake Lark API must have received the confirmation
	// reply. Either a /reply (anchor on trigger) or a /messages (new
	// thread) is acceptable — both are valid create-issue confirmation
	// paths depending on whether thread_id was set. The body must be an
	// interactive card that mentions the issue title. We narrow to POST
	// because the same prefix also serves GET (thread history) and the
	// history call carries no card payload.
	confirm := fake.FindRequestByMethod(http.MethodPost, "/im/v1/messages")
	if confirm == nil {
		t.Fatalf("fake Lark API received no POST /im/v1/messages call after create-issue webhook\nrequests=%+v",
			fake.CapturedRequests())
	}

	var payload map[string]any
	if err := json.Unmarshal(confirm.Body, &payload); err != nil {
		t.Fatalf("confirmation payload not JSON: %v body=%s", err, string(confirm.Body))
	}
	// The current implementation sends create-issue confirmation as a
	// plain text reply (msg_type=text). If a future change switches to
	// an interactive card both are acceptable — the contract this test
	// pins is "the issue title surfaces in the reply", not the rendering
	// strategy.
	mt, _ := payload["msg_type"].(string)
	if mt != "text" && mt != "interactive" {
		t.Errorf("confirmation msg_type = %q, want text or interactive", mt)
	}
	contentStr, _ := payload["content"].(string)
	if contentStr == "" {
		t.Fatalf("confirmation has empty content payload")
	}
	if !strings.Contains(contentStr, issueTitle) {
		t.Errorf("confirmation does not mention issue title %q\ncontent=%s", issueTitle, contentStr)
	}
}

// TestIntegration_LarkThread_CreateIssueWebhook_LinkedDocFromBodyExpanded
// is the bridge between scenarios 1 and 4: a "@bot 创建任务" command
// whose payload puts a Lark docx URL in the issue body should produce
// an issue whose claim-time expansion fetches that doc via the same
// fake Lark API the webhook already used.
//
// The body-vs-title split is important: titleAndDescription takes the
// first line as the title and everything after as the description, so
// to exercise the description-scanning path we send a two-line message
// (title on line 1, URL on line 2). The Lark webhook accepts JSON-
// encoded "\n" inside the content's text field.
func TestIntegration_LarkThread_CreateIssueWebhook_LinkedDocFromBodyExpanded(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)

	const docxBody = "Spec body the agent must read"
	const docxURL = "https://acme.feishu.cn/docx/E2eBridgeDoc"

	fake := newFakeLarkAPI(t)
	fake.Handle("/docx/v1/documents/E2eBridgeDoc/raw_content", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"content": docxBody},
		})
	})
	installFakeLarkThreadServiceCapturing(t, fake)
	installFakeLarkDocs(t, fake)

	const openID = "ou_e2e_bridge"
	const chatID = "oc_e2e_bridge"
	const triggerMsgID = "om_e2e_bridge_trigger"
	seedLarkChatBinding(t, chatID)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	// Two-line payload: title on line 1 stays inside titleAndDescription's
	// title slot, the URL on line 2 lands in the description where
	// fetchLinkedDocsForIssue scans for it. We use a JSON-escaped "\n"
	// (backslash + n in the raw Go string) so the webhook helper produces
	// a well-formed JSON content string that decodes to a real newline.
	body := buildLarkV2MessageEvent(openID, chatID, "", triggerMsgID,
		`@_user_1 创建任务 implement the linked spec\nrefs: `+docxURL)
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("webhook status = %d body = %s", rr.Code, rr.Body.String())
	}

	// Locate the freshly-created issue and confirm its description
	// carries the docx URL so the fetcher can find it on claim.
	var issue db.Issue
	{
		var id string
		if err := testPool.QueryRow(context.Background(),
			`SELECT id FROM issue WHERE workspace_id = $1 AND creator_id::text = $2
			 ORDER BY created_at DESC LIMIT 1`,
			testWorkspaceID, testUserID).Scan(&id); err != nil {
			t.Fatalf("read back issue: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id) })
		var err error
		issue, err = testHandler.Queries.GetIssue(context.Background(), parseUUID(id))
		if err != nil {
			t.Fatalf("reload issue: %v", err)
		}
	}
	if !issue.Description.Valid || !strings.Contains(issue.Description.String, docxURL) {
		t.Fatalf("issue description should carry docx URL, got %q", issue.Description.String)
	}

	// Now drive the claim-time expansion path. The fake API was set up
	// before the webhook fired, so the same fake handles both calls —
	// and the assertion is on the LinkedDoc content the daemon would
	// see on its claim response.
	docs := testHandler.fetchLinkedDocsForIssue(context.Background(), issue)
	if len(docs) != 1 || docs[0].URL != docxURL || docs[0].Content != docxBody {
		t.Fatalf("end-to-end docx expansion failed: docs=%+v", docs)
	}

	// And verify the wire-level chain: webhook → confirmation reply +
	// docx fetch both reached the same fake Lark API.
	if fake.FindRequestByMethod(http.MethodPost, "/im/v1/messages") == nil {
		t.Errorf("missing confirmation reply on fake Lark API")
	}
	if fake.FindRequest("/docx/v1/documents/E2eBridgeDoc/raw_content") == nil {
		t.Errorf("missing docx fetch on fake Lark API")
	}
}
