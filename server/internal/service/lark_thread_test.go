package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseLarkBotVerb_CreateIssueChinese(t *testing.T) {
	verb, rest := ParseLarkBotVerb("创建任务 implement feature X")
	if verb != LarkVerbCreateIssue {
		t.Fatalf("verb = %q, want %q", verb, LarkVerbCreateIssue)
	}
	if rest != "implement feature X" {
		t.Fatalf("rest = %q, want %q", rest, "implement feature X")
	}
}

func TestParseLarkBotVerb_CreateIssueEnglishAliases(t *testing.T) {
	for _, alias := range []string{"create_issue task body", "create-issue task body"} {
		verb, rest := ParseLarkBotVerb(alias)
		if verb != LarkVerbCreateIssue {
			t.Fatalf("alias %q → verb %q, want %q", alias, verb, LarkVerbCreateIssue)
		}
		if rest != "task body" {
			t.Fatalf("alias %q → rest %q, want %q", alias, rest, "task body")
		}
	}
}

func TestParseLarkBotVerb_LeadingWhitespaceStripped(t *testing.T) {
	verb, rest := ParseLarkBotVerb("  \n创建任务  body  ")
	if verb != LarkVerbCreateIssue {
		t.Fatalf("verb = %q, want %q", verb, LarkVerbCreateIssue)
	}
	if rest != "body" {
		t.Fatalf("rest = %q, want %q", rest, "body")
	}
}

func TestParseLarkBotVerb_NoVerb(t *testing.T) {
	verb, rest := ParseLarkBotVerb("hello world, just chatting")
	if verb != LarkVerbNone {
		t.Fatalf("verb = %q, want %q", verb, LarkVerbNone)
	}
	if rest != "" {
		t.Fatalf("rest = %q, want empty for non-verb input", rest)
	}
}

// Regression: 创建任务 as a substring of a longer Chinese token must NOT
// trigger the verb — otherwise "创建任务模板更新了" would parse as
// create_issue with body "模板更新了". This is the no-misfires guard
// the design's "no NLU" rule depends on.
func TestParseLarkBotVerb_NoFalseMatchOnSubstring(t *testing.T) {
	verb, _ := ParseLarkBotVerb("创建任务模板更新了")
	if verb != LarkVerbNone {
		t.Fatalf("verb = %q, want %q (substring must not match)", verb, LarkVerbNone)
	}
}

func TestParseLarkBotVerb_ReservedVerbsRecognised(t *testing.T) {
	cases := map[string]LarkBotVerb{
		"link-doc https://example":   LarkVerbLinkDoc,
		"link_doc https://example":   LarkVerbLinkDoc,
		"open-meeting next thursday": LarkVerbOpenMeeting,
		"open_meeting next thursday": LarkVerbOpenMeeting,
	}
	for text, want := range cases {
		got, _ := ParseLarkBotVerb(text)
		if got != want {
			t.Errorf("ParseLarkBotVerb(%q) = %q, want %q", text, got, want)
		}
	}
}

func TestStripLarkMentionPlaceholders_RemovesUserPlaceholders(t *testing.T) {
	in := "@_user_1 创建任务 ping @_user_42 about deploy"
	want := "创建任务 ping about deploy"
	if got := StripLarkMentionPlaceholders(in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStripLarkMentionPlaceholders_KeepsRealAtSymbols(t *testing.T) {
	// `@channel` (not Lark's auto-mention format) should pass through;
	// only `@_user_N` placeholders are scrubbed.
	in := "@_user_1 ping team@multica.ai"
	want := "ping team@multica.ai"
	if got := StripLarkMentionPlaceholders(in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestStripLarkMentionPlaceholders_CollapsesDoubleSpaces(t *testing.T) {
	in := "@_user_1  hello  @_user_2  world"
	want := "hello world"
	if got := StripLarkMentionPlaceholders(in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLarkThreadContext_TitleAndDescription_FromBodyOnly(t *testing.T) {
	tc := &LarkThreadContext{Body: "fix login bug\nrepro: try ssoing in twice"}
	title, desc := tc.titleAndDescription()
	if title != "fix login bug" {
		t.Fatalf("title = %q", title)
	}
	if !strings.Contains(desc, "repro: try ssoing in twice") {
		t.Fatalf("desc missing remainder: %q", desc)
	}
}

func TestLarkThreadContext_TitleAndDescription_FromThreadFallback(t *testing.T) {
	tc := &LarkThreadContext{
		Body: "",
		ThreadMessages: []LarkThreadMessage{
			{Text: "@_user_1 we need to triage flaky CI", SenderOpenID: "ou_a", CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)},
			{Text: "agree, let's open one", SenderOpenID: "ou_b", CreatedAt: time.Date(2026, 5, 1, 12, 1, 0, 0, time.UTC)},
		},
	}
	title, desc := tc.titleAndDescription()
	if title != "we need to triage flaky CI" {
		t.Fatalf("title = %q", title)
	}
	if !strings.Contains(desc, "12:00") || !strings.Contains(desc, "12:01") {
		t.Fatalf("desc missing timestamps: %q", desc)
	}
	if !strings.Contains(desc, "we need to triage flaky CI") {
		t.Fatalf("desc missing first message: %q", desc)
	}
}

func TestLarkThreadContext_TitleAndDescription_GenericFallback(t *testing.T) {
	tc := &LarkThreadContext{}
	title, desc := tc.titleAndDescription()
	if title != "Lark thread issue" {
		t.Fatalf("title = %q, want generic fallback", title)
	}
	if desc != "" {
		t.Fatalf("desc = %q, want empty when nothing to render", desc)
	}
}

func TestLarkThreadContext_TitleTruncated(t *testing.T) {
	long := strings.Repeat("x", LarkIssueTitleMaxRunes+10)
	tc := &LarkThreadContext{Body: long}
	title, _ := tc.titleAndDescription()
	if !strings.HasSuffix(title, "…") {
		t.Fatalf("expected ellipsis on truncated title, got %q", title)
	}
}

// ── HTTP-mock tests: ListThreadMessages + ReplyToMessage ────────────────

func TestLarkClient_ListThreadMessages_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		if !strings.Contains(r.URL.Path, "/im/v1/messages") {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		if r.URL.Query().Get("container_id_type") != "thread" {
			t.Errorf("missing container_id_type=thread")
		}
		writeJSONResp(w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"items": []map[string]any{
					{
						"message_id":   "om_root",
						"create_time":  "1747555200000",
						"msg_type":     "text",
						"body":         map[string]any{"content": `{"text":"first message"}`},
						"sender":       map[string]any{"id": "ou_alice", "id_type": "open_id"},
					},
					{
						"message_id":   "om_reply",
						"create_time":  "1747555260000",
						"msg_type":     "text",
						"body":         map[string]any{"content": `{"text":"reply text"}`},
						"sender":       map[string]any{"id": "ou_bob", "id_type": "open_id"},
					},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := NewLarkClient(LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "c", EncryptKey: "d"})
	SetAPIBaseForTest(c, srv.URL)

	got, err := c.ListThreadMessages(context.Background(), "omt_xyz", 10)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0].Text != "first message" || got[1].Text != "reply text" {
		t.Fatalf("text mismatch: %+v", got)
	}
	if got[0].SenderOpenID != "ou_alice" {
		t.Fatalf("sender mismatch: %+v", got[0])
	}
	if got[0].CreatedAt.IsZero() {
		t.Fatalf("create_time not parsed: %+v", got[0])
	}
}

func TestLarkClient_ListThreadMessages_SkipsNonTextWithoutErroring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		writeJSONResp(w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"items": []map[string]any{
					{
						"message_id": "om_text",
						"msg_type":   "text",
						"body":       map[string]any{"content": `{"text":"hi"}`},
						"sender":     map[string]any{"id": "ou_a"},
					},
					{
						"message_id": "om_image",
						"msg_type":   "image",
						"body":       map[string]any{"content": `{"image_key":"abc"}`},
						"sender":     map[string]any{"id": "ou_b"},
					},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	c := NewLarkClient(LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "c", EncryptKey: "d"})
	SetAPIBaseForTest(c, srv.URL)

	got, err := c.ListThreadMessages(context.Background(), "omt_xyz", 10)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both messages returned, got %d", len(got))
	}
	if got[0].Text != "hi" {
		t.Fatalf("text 0 = %q, want hi", got[0].Text)
	}
	// Non-text message has empty Text — the caller (titleAndDescription)
	// skips empty entries in the transcript. We deliberately keep the
	// MessageID so the original ordering is preserved.
	if got[1].Text != "" {
		t.Fatalf("text 1 = %q, want empty for image", got[1].Text)
	}
}

func TestLarkClient_ReplyToMessage_PostsExpectedShape(t *testing.T) {
	var gotPath string
	var gotPayload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotPayload)
		writeJSONResp(w, map[string]any{"code": 0})
	}))
	t.Cleanup(srv.Close)

	c := NewLarkClient(LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "c", EncryptKey: "d"})
	SetAPIBaseForTest(c, srv.URL)

	if err := c.ReplyToMessage(context.Background(), "om_abc", "hello"); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasSuffix(gotPath, "/im/v1/messages/om_abc/reply") {
		t.Fatalf("path = %q", gotPath)
	}
	if gotPayload["msg_type"] != "text" {
		t.Fatalf("msg_type = %v, want text", gotPayload["msg_type"])
	}
	if gotPayload["reply_in_thread"] != true {
		t.Fatalf("reply_in_thread = %v, want true", gotPayload["reply_in_thread"])
	}
	contentStr, _ := gotPayload["content"].(string)
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(contentStr), &content); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if content.Text != "hello" {
		t.Fatalf("content.text = %q, want hello", content.Text)
	}
}

func TestLarkThreadService_Configured_NilSafe(t *testing.T) {
	var s *LarkThreadService
	if s.Configured() {
		t.Fatal("nil service should not be configured")
	}
	s2 := &LarkThreadService{}
	if s2.Configured() {
		t.Fatal("service with nil client should not be configured")
	}
}
