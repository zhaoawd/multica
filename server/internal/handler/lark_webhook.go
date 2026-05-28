package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Lark webhook (P2.2) — `POST /api/webhooks/lark`.
//
// Two payload shapes land here:
//
//  1. URL verification handshake (Lark posts this once when the operator
//     configures the callback URL in the Lark developer console):
//         {"type":"url_verification","token":"...","challenge":"..."}
//     We echo the challenge back verbatim.
//
//  2. Interactive-card action callbacks (a user clicked a button on a
//     card the bot posted):
//         {"open_id":"ou_...","action":{"value":{"verb":"claim","issue_id":"..."}}, ...}
//     We dispatch on the verb, mutate the issue, and return a Lark toast.
//
// The endpoint is public — Lark cannot authenticate with a multica
// session — so trust comes entirely from:
//   (a) `X-Lark-Signature` header (preferred when present): the SHA-256 of
//       `timestamp + nonce + LARK_ENCRYPT_KEY + body`, base64-encoded.
//   (b) Body `token` field falling back to LARK_VERIFICATION_TOKEN
//       (covers the url_verification step which doesn't sign).
//
// Both checks are constant-time. Any failure → 401.

// LarkCardCallback is the subset of fields we parse from card action
// callbacks. Lark sends more (tenant_key, open_message_id, timezone,
// open_chat_id, ...) but P2.2 doesn't need them yet — staying minimal
// keeps the parser tolerant to schema drift.
type LarkCardCallback struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	Challenge string `json:"challenge"`
	OpenID    string `json:"open_id"`
	Action    struct {
		Tag   string         `json:"tag"`
		Value map[string]any `json:"value"`
	} `json:"action"`
}

// HandleLarkWebhook routes the request to the right inner handler. Always
// returns 200 + JSON so Lark doesn't retry; security failures come back
// as 401 *before* any business logic runs.
//
// Three envelope shapes coexist on this endpoint:
//
//	v1 url_verification    — flat {"type":"url_verification","challenge":"..."}
//	v1 card action         — {"action":{"tag":"button",...}}
//	v2 event subscription  — {"schema":"2.0","header":{"event_type":"..."},"event":{...}}
//
// When LarkCallbackMode is "websocket", message events (v2) are skipped
// here because they arrive via the WS client. Card callbacks (v1) are
// still handled here even in websocket mode — the oapi-sdk-go WS client
// does not dispatch MessageTypeCard, so card buttons must flow through
// this HTTP path regardless of callback mode.
func (h *Handler) HandleLarkWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}

	if !verifyLarkWebhook(r, body) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	// Minimal sniff. Keep it tolerant — Lark's v1 and v2 envelopes both
	// have `Type` at the top level under certain configurations, so the
	// branch order below matters.
	var envelope struct {
		Schema    string `json:"schema"`
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Header    struct {
			EventType string `json:"event_type"`
		} `json:"header"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}

	// v1 URL verification handshake (Lark posts once at endpoint setup).
	// Always handled regardless of callback mode — the operator may need
	// to verify the URL even in websocket mode.
	if envelope.Type == "url_verification" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": envelope.Challenge})
		return
	}

	wsMode := strings.EqualFold(h.LarkCallbackMode, "websocket")

	// v2 event subscription — currently we only dispatch on message
	// receive (P4 @bot verbs). Other v2 events are silently ack'd.
	if envelope.Schema == "2.0" {
		if !wsMode && envelope.Header.EventType == "im.message.receive_v1" {
			h.handleLarkMessageEvent(r.Context(), w, body)
			return
		}
		// In websocket mode, message events arrive via the WS client.
		// Ack here so Lark doesn't retry if an operator accidentally
		// left the HTTP callback configured alongside the WS client.
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	// v1 interactive-card callback. The oapi-sdk-go WS client does not
	// dispatch MessageTypeCard (it returns early), so card callbacks
	// must always go through the HTTP path even in websocket mode.
	var payload LarkCardCallback
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}
	if payload.Action.Tag != "button" {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}

	h.dispatchLarkCardAction(r, w, payload)
}

// verifyLarkWebhook is true when:
//   - X-Lark-Signature header matches base64(SHA256(ts|nonce|key|body)), OR
//   - the body's "token" field equals LARK_VERIFICATION_TOKEN.
//
// At least one path must succeed; the second is the fallback for the
// url_verification handshake (Lark doesn't sign that POST when the
// app's "Encrypt Key" is empty in some configurations).
func verifyLarkWebhook(r *http.Request, body []byte) bool {
	encryptKey := strings.TrimSpace(os.Getenv("LARK_ENCRYPT_KEY"))
	verifyToken := strings.TrimSpace(os.Getenv("LARK_VERIFICATION_TOKEN"))
	if encryptKey == "" || verifyToken == "" {
		return false
	}

	if sig := r.Header.Get("X-Lark-Signature"); sig != "" {
		ts := r.Header.Get("X-Lark-Request-Timestamp")
		nonce := r.Header.Get("X-Lark-Request-Nonce")
		h := sha256.New()
		h.Write([]byte(ts))
		h.Write([]byte(nonce))
		h.Write([]byte(encryptKey))
		h.Write(body)
		expected := base64.StdEncoding.EncodeToString(h.Sum(nil))
		if hmac.Equal([]byte(expected), []byte(sig)) {
			return true
		}
		// Signature present but did NOT match → reject rather than
		// silently falling through to the token check. Mixed-success
		// matching is how scheme-confusion attacks land.
		return false
	}

	// No signature header — fall back to the in-body token. We parse
	// just the token field so a malformed body still fails closed.
	var probe struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return hmac.Equal([]byte(probe.Token), []byte(verifyToken))
}

// larkToast is the Lark response shape that surfaces an ephemeral
// notification to the clicker. Type ∈ {"info","success","warning","error"}.
type larkToast struct {
	Toast struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	} `json:"toast"`
}

func toastResponse(kind, msg string) larkToast {
	var t larkToast
	t.Toast.Type = kind
	t.Toast.Content = msg
	return t
}

// dispatchLarkCardAction is where the actual business logic lives.
// Errors are mapped to Lark toasts (so the user sees what failed) rather
// than HTTP errors (Lark would retry on 5xx and we don't want duplicate
// claims).
func (h *Handler) dispatchLarkCardAction(r *http.Request, w http.ResponseWriter, payload LarkCardCallback) {
	toast := h.processLarkCardAction(r.Context(), payload.OpenID, payload.Action.Value)
	writeJSON(w, http.StatusOK, toast)
}

// LarkCardActionResult holds the verb, value map from a card callback.
// Used by both the HTTP webhook path and the WebSocket long-connection path.
type LarkCardActionResult struct {
	OpenID string
	Value  map[string]any
}

// processLarkCardAction executes the card action business logic and
// returns a toast response. Transport-agnostic — called by both the
// HTTP webhook handler and the WebSocket event bridge.
func (h *Handler) processLarkCardAction(ctx context.Context, openID string, value map[string]any) larkToast {
	verb, _ := value["verb"].(string)
	issueIDStr, _ := value["issue_id"].(string)
	if verb == "" || issueIDStr == "" {
		return toastResponse("error", "missing verb or issue_id")
	}

	if openID == "" {
		return toastResponse("error", "missing open_id")
	}
	link, err := h.Queries.GetLarkUserLinkByOpenID(ctx, openID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return toastResponse("warning",
				"Link your Lark account in Multica → Settings → Profile to act on cards.")
		}
		slog.Warn("lark webhook: lookup user link", "err", err, "open_id", openID)
		return toastResponse("error", "internal error")
	}

	issueUUID, err := parseStrictUUID(issueIDStr)
	if err != nil {
		return toastResponse("error", "bad issue id")
	}
	issue, err := h.Queries.GetIssue(ctx, issueUUID)
	if err != nil {
		return toastResponse("error", "issue not found")
	}

	if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      link.UserID,
		WorkspaceID: issue.WorkspaceID,
	}); err != nil {
		return toastResponse("warning", "You are not a member of this workspace.")
	}

	switch verb {
	case "claim":
		return h.larkClaimIssueCore(ctx, issue, link.UserID)
	case "mark_done":
		return h.larkMarkIssueDoneCore(ctx, issue)
	default:
		return toastResponse("error", "unsupported action")
	}
}

// larkClaimIssueCore assigns the issue to the clicking user and returns
// a toast. Transport-agnostic. Rejects the claim if the issue already
// has an assignee — stale Claim cards from before someone else claimed
// the issue must not silently reassign.
func (h *Handler) larkClaimIssueCore(ctx context.Context, issue db.Issue, userID pgtype.UUID) larkToast {
	if issue.AssigneeID.Valid {
		return toastResponse("info", "Already claimed by someone else")
	}
	updated, err := h.Queries.UpdateIssue(ctx, db.UpdateIssueParams{
		ID:            issue.ID,
		AssigneeType:  pgtype.Text{String: "member", Valid: true},
		AssigneeID:    userID,
		DueDate:       issue.DueDate,
		ParentIssueID: issue.ParentIssueID,
		ProjectID:     issue.ProjectID,
	})
	if err != nil {
		slog.Warn("lark webhook: claim failed", "err", err, "issue_id", uuidToString(issue.ID))
		return toastResponse("error", "failed to claim")
	}
	wsID := uuidToString(issue.WorkspaceID)
	prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
	resp := issueToResponse(updated, prefix)
	h.publish(protocol.EventIssueUpdated, wsID, "member", uuidToString(userID), map[string]any{
		"issue":            resp,
		"assignee_changed": true,
		"prev_assignee_id": uuidToString(issue.AssigneeID),
		"source":           "lark_card_action",
	})
	return toastResponse("success", "Claimed")
}

// larkMarkIssueDoneCore moves the issue to "done" and returns a toast.
// Transport-agnostic.
func (h *Handler) larkMarkIssueDoneCore(ctx context.Context, issue db.Issue) larkToast {
	if issue.Status == "done" {
		return toastResponse("info", "Already done")
	}
	updated, err := h.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID:          issue.ID,
		Status:      "done",
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		slog.Warn("lark webhook: mark done failed", "err", err, "issue_id", uuidToString(issue.ID))
		return toastResponse("error", "failed to mark done")
	}
	wsID := uuidToString(issue.WorkspaceID)
	prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
	resp := issueToResponse(updated, prefix)
	h.publish(protocol.EventIssueUpdated, wsID, "system", "", map[string]any{
		"issue":          resp,
		"status_changed": true,
		"prev_status":    issue.Status,
		"source":         "lark_card_action",
	})
	return toastResponse("success", "Marked done")
}

// ── P4: message receive event dispatch ─────────────────────────────────

// larkMessageEvent is the subset of Lark's im.message.receive_v1 payload
// the @bot dispatcher consumes. Lark sends much more (event_id,
// tenant_key, encrypt-mode metadata, message i18n hints, ...) but P4
// only needs the sender identity, message id triple, and content.
//
// Mentions is the array Lark fills with the entities @-tagged in the
// message. We don't filter on which entry is "our bot" — Lark's event
// subscription scope `im.message.receive_v1` only delivers messages
// where the bot is already a meaningful participant (direct messages
// and group messages that @-mention the bot or hit a configured
// keyword), so any inbound payload counts as a bot-addressed message
// by construction.
type larkMessageEvent struct {
	Schema string `json:"schema"`
	Event  struct {
		Sender struct {
			SenderType string `json:"sender_type"`
			SenderID   struct {
				OpenID  string `json:"open_id"`
				UnionID string `json:"union_id"`
			} `json:"sender_id"`
		} `json:"sender"`
		Message struct {
			MessageID  string `json:"message_id"`
			RootID     string `json:"root_id"`
			ParentID   string `json:"parent_id"`
			ThreadID   string `json:"thread_id"`
			ChatID     string `json:"chat_id"`
			ChatType   string `json:"chat_type"`
			MsgType    string `json:"message_type"`
			Content    string `json:"content"`
			CreateTime string `json:"create_time"`
		} `json:"message"`
	} `json:"event"`
}

// handleLarkMessageEvent routes a Lark message event to the right verb
// handler. The response body is always an empty object — Lark v2 events
// don't use the response payload, and a non-200 would just trigger a
// retry storm. Errors surface back to the user as Lark thread replies
// (see replyToLarkMessage), never as HTTP errors.
func (h *Handler) handleLarkMessageEvent(ctx context.Context, w http.ResponseWriter, body []byte) {
	defer writeJSON(w, http.StatusOK, map[string]any{})

	var evt larkMessageEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		slog.Warn("lark @bot: unmarshal event failed", "err", err)
		return
	}
	h.ProcessLarkMessageEvent(ctx, evt)
}

// ProcessLarkMessageEvent is the transport-agnostic core of message
// event handling. Called by both the HTTP webhook path and the WebSocket
// long-connection bridge.
func (h *Handler) ProcessLarkMessageEvent(ctx context.Context, evt larkMessageEvent) {
	if h.LarkThread == nil || !h.LarkThread.Configured() {
		slog.Info("lark @bot: integration unconfigured, skipping message event")
		return
	}

	msg := evt.Event.Message
	if msg.MessageID == "" || msg.ChatID == "" {
		return
	}

	if evt.Event.Sender.SenderType != "user" || msg.MsgType != "text" {
		return
	}

	rawText := extractLarkTextContent(msg.Content)
	if rawText == "" {
		return
	}
	cleaned := service.StripLarkMentionPlaceholders(rawText)

	if slash := parseLarkSlashCommand(cleaned); slash != larkSlashNone {
		h.handleLarkSlashCommand(ctx, msg, evt.Event.Sender.SenderID.OpenID, slash)
		return
	}

	verb, remainder := service.ParseLarkBotVerb(cleaned)
	if verb == service.LarkVerbNone {
		if evt.Event.Message.RootID != "" {
			h.handleLarkInboundReply(ctx, msg, evt.Event.Sender.SenderID.OpenID, cleaned)
		}
		return
	}

	binding, err := h.Queries.GetLarkWorkspaceBindingByChatID(ctx, msg.ChatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.Info("lark @bot: chat not bound to any workspace", "chat_id", msg.ChatID)
			return
		}
		slog.Warn("lark @bot: workspace binding lookup", "err", err, "chat_id", msg.ChatID)
		return
	}

	if evt.Event.Sender.SenderID.OpenID == "" {
		return
	}
	link, err := h.Queries.GetLarkUserLinkByOpenID(ctx, evt.Event.Sender.SenderID.OpenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.replyToLarkMessage(ctx, msg.MessageID,
				"在 Multica → Settings → Profile 绑定飞书账号后即可使用 @bot 指令。")
			return
		}
		slog.Warn("lark @bot: user link lookup", "err", err)
		return
	}

	if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      link.UserID,
		WorkspaceID: binding.WorkspaceID,
	}); err != nil {
		h.replyToLarkMessage(ctx, msg.MessageID, "你不是该 workspace 的成员，无法执行此指令。")
		return
	}

	switch verb {
	case service.LarkVerbCreateIssue:
		h.handleLarkCreateIssue(ctx, msg, link.UserID, binding.WorkspaceID, remainder)
	case service.LarkVerbLinkDoc, service.LarkVerbOpenMeeting:
		h.replyToLarkMessage(ctx, msg.MessageID, "该指令暂未实装，敬请期待。")
	}
}

// handleLarkCreateIssue runs the @bot 创建任务 verb end-to-end:
// fetch thread context (best effort), insert the issue + link row in a
// single transaction, and reply into the originating Lark thread with
// the new issue identifier.
//
// On failure we reply with a short error string. The reply itself is
// also best-effort — if Lark is down, the issue still exists in
// multica and the user can navigate to it from the web UI.
func (h *Handler) handleLarkCreateIssue(ctx context.Context, msg larkInboundMessage, creator pgtype.UUID, workspaceID pgtype.UUID, body string) {
	// Choose the bridge anchor. Prefer thread_id (a real Lark "thread"),
	// then root_id (a reply to a non-threaded message — still useful for
	// inbound bridging), then the trigger message itself. Pulling the
	// thread transcript only makes sense when thread_id is set; for
	// root_id we still record the anchor but skip the list call.
	threadID := msg.ThreadID
	if threadID == "" && msg.RootID != "" {
		// Reply-to-message anchor — we keep the link pointing at the
		// root so future replies still bridge correctly, but don't
		// burn an API call trying to list a non-thread container.
		threadID = ""
	}

	tc := h.LarkThread.FetchThreadContext(ctx, msg.ChatID, msg.MessageID, threadID, body)
	if msg.RootID != "" && tc.ThreadID == "" {
		// Anchor the bridge on the conversation root the user replied
		// to, even though we didn't fetch a transcript.
		tc.RootMessageID = msg.RootID
	}

	issue, err := h.LarkThread.CreateIssueFromThread(ctx, workspaceID, creator, tc)
	if err != nil {
		slog.Warn("lark @bot: create issue failed",
			"err", err,
			"chat_id", msg.ChatID,
			"workspace_id", uuidToString(workspaceID),
		)
		h.replyToLarkMessage(ctx, msg.MessageID, "创建 issue 失败，请稍后再试。")
		return
	}

	// §14.1.3: download thread media into issue attachments. Best-
	// effort — issue exists regardless of what landed. Permission
	// denied surfaces as a throttled bot reply asking the admin
	// to grant im:resource; other failures show up as inline
	// placeholder lines that future edits can leave in the body.
	if h.LarkMedia != nil && h.LarkMedia.Configured() {
		report := h.LarkMedia.DownloadAndAttach(ctx, workspaceID, "member", creator, issue.ID, tc)
		if report.PermissionDenied {
			h.LarkMedia.EmitPermissionWarning(ctx, workspaceID, tc.RootMessageID)
		}
		if notices := report.InlineNotices(); len(notices) > 0 {
			h.appendNoticesToIssueDescription(ctx, issue.ID, notices)
		}
	}

	prefix := h.getIssuePrefix(ctx, workspaceID)
	identifier := fmt.Sprintf("%s-%d", prefix, issue.Number)
	h.LarkThread.ReplyWithIssueCreated(ctx, tc, identifier, issue.Title)
}

// appendNoticesToIssueDescription tacks the §14.1.3 placeholder lines
// onto the issue description. Best-effort: a failed DB write logs and
// returns — the issue already exists, the missing attachments are
// already absent, and the reviewer will still see the surviving
// attachments in the multica issue page.
func (h *Handler) appendNoticesToIssueDescription(ctx context.Context, issueID pgtype.UUID, notices []string) {
	if len(notices) == 0 {
		return
	}
	issue, err := h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		slog.Warn("lark @bot: refetch issue for notices failed", "err", err)
		return
	}
	var b strings.Builder
	if issue.Description.Valid {
		b.WriteString(issue.Description.String)
		if !strings.HasSuffix(issue.Description.String, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	for _, n := range notices {
		b.WriteString(n)
		b.WriteString("\n")
	}
	updated := strings.TrimRight(b.String(), "\n")
	if _, err := h.Queries.UpdateIssue(ctx, db.UpdateIssueParams{
		ID:            issue.ID,
		Description:   pgtype.Text{String: updated, Valid: updated != ""},
		AssigneeType:  issue.AssigneeType,
		AssigneeID:    issue.AssigneeID,
		DueDate:       issue.DueDate,
		ParentIssueID: issue.ParentIssueID,
		ProjectID:     issue.ProjectID,
	}); err != nil {
		slog.Warn("lark @bot: append notices to issue description failed",
			"err", err, "issue_id", uuidToString(issue.ID))
	}
}

// larkInboundMessage is the projected subset of larkMessageEvent.Event.Message
// that handleLarkCreateIssue (and any future verb handler) accepts. Defined
// here rather than reusing the anonymous struct field type so the helper
// is reusable from tests without rebuilding the full envelope.
type larkInboundMessage = struct {
	MessageID  string `json:"message_id"`
	RootID     string `json:"root_id"`
	ParentID   string `json:"parent_id"`
	ThreadID   string `json:"thread_id"`
	ChatID     string `json:"chat_id"`
	ChatType   string `json:"chat_type"`
	MsgType    string `json:"message_type"`
	Content    string `json:"content"`
	CreateTime string `json:"create_time"`
}

// extractLarkTextContent unwraps Lark's text-message content envelope
// (`{"text":"..."}`) into a plain string. Returns "" for non-text or
// malformed payloads — callers treat that as "nothing to dispatch on".
func extractLarkTextContent(content string) string {
	if content == "" {
		return ""
	}
	var inner struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &inner); err != nil {
		return ""
	}
	return strings.TrimSpace(inner.Text)
}

// replyToLarkMessage posts a plain-text reply, swallowing the error. The
// caller already decided to send a message in response to a user
// action; if the reply fails we log and move on rather than retrying
// (Lark deduplicates by message_id + reply chain, so a retry could
// produce a duplicate reply).
func (h *Handler) replyToLarkMessage(ctx context.Context, messageID, text string) {
	if h.LarkThread == nil || h.LarkThread.Client == nil || messageID == "" {
		return
	}
	if err := h.LarkThread.Client.ReplyToMessage(ctx, messageID, text); err != nil {
		slog.Warn("lark @bot: reply failed", "err", err, "message_id", messageID)
	}
}

// ── P5: inbound clarification bridge (thread reply → multica comment) ──

// handleLarkInboundReply lands a Lark thread reply as a multica comment
// on the issue the thread is bridged to. The anchor for the lookup is
// the message's `root_id` field, which Lark sets to the original thread
// root for every reply (regardless of whether the user replied to the
// root directly or to a later message in the chain).
//
// The bridge fires only when:
//   - The thread root is in lark_issue_link (i.e. the issue actually
//     came from this thread — we don't try to guess for unrelated
//     threads the bot happens to be in).
//   - The chat is still bound to the same workspace (an operator who
//     unbinds the chat is opting out of the bridge for future replies).
//   - The sender has a lark_user_link and is a member of the workspace.
//
// Failures at every step are silent: no error reply into the chat. A
// Lark user who happens to reply in a bridged thread without being
// linked is not abusing the bot, and an automated error message would
// be noisier than just dropping. The @bot create-issue path still
// surfaces the "bind your account" hint, which is the intended
// onboarding nudge.
func (h *Handler) handleLarkInboundReply(ctx context.Context, msg larkInboundMessage, senderOpenID, content string) {
	if h.TaskService == nil || senderOpenID == "" || msg.RootID == "" {
		return
	}
	body := strings.TrimSpace(content)
	if body == "" {
		return
	}

	link, err := h.Queries.GetLarkIssueLinkByRootMessage(ctx, msg.RootID)
	if err != nil {
		// pgx.ErrNoRows is the common case: a reply in an unrelated
		// thread that the bot happens to be in. Other errors are also
		// non-fatal — the bridge is best-effort.
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("lark inbound: link lookup", "err", err, "root_id", msg.RootID)
		}
		return
	}
	// Sanity: the link row's chat_id must match the message's chat. A
	// mismatch could mean the thread root was somehow attributed to a
	// different chat than the one we're now seeing replies in. Drop
	// rather than misroute the comment.
	if link.ChatID != msg.ChatID {
		slog.Warn("lark inbound: chat_id mismatch",
			"link_chat", link.ChatID, "msg_chat", msg.ChatID, "root", msg.RootID)
		return
	}

	// Load the linked issue so we have workspace + assignee context.
	issue, err := h.Queries.GetIssue(ctx, link.IssueID)
	if err != nil {
		slog.Warn("lark inbound: issue lookup", "err", err, "issue_id", uuidToString(link.IssueID))
		return
	}

	// Chat binding must still exist and point at the same workspace.
	// This is the operator-controlled gate: unbinding the chat in
	// settings turns the bridge off going forward, even though the
	// historical lark_issue_link row remains.
	binding, err := h.Queries.GetLarkWorkspaceBindingByChatID(ctx, msg.ChatID)
	if err != nil {
		return
	}
	if uuidToString(binding.WorkspaceID) != uuidToString(issue.WorkspaceID) {
		slog.Warn("lark inbound: binding workspace drift",
			"binding_ws", uuidToString(binding.WorkspaceID),
			"issue_ws", uuidToString(issue.WorkspaceID))
		return
	}

	// Resolve sender → multica member.
	userLink, err := h.Queries.GetLarkUserLinkByOpenID(ctx, senderOpenID)
	if err != nil {
		// Unlinked Lark users can still chat in the thread freely — we
		// just don't bridge their replies. No "bind your account" reply
		// here on purpose: this is not an @bot invocation, the user
		// didn't ask for anything from multica.
		return
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      userLink.UserID,
		WorkspaceID: issue.WorkspaceID,
	}); err != nil {
		return
	}

	// Create the comment. Author is the resolved member; type defaults
	// to "comment". No parent — the bridge always lands at the top
	// level so the agent's on_comment trigger reads it as a fresh
	// reply to the issue.
	comment, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "member",
		AuthorID:    userLink.UserID,
		Content:     body,
		Type:        "comment",
		ParentID:    pgtype.UUID{},
	})
	if err != nil {
		slog.Warn("lark inbound: create comment failed",
			"err", err, "issue_id", uuidToString(issue.ID))
		return
	}

	// Project the new comment into the response shape so realtime
	// consumers receive the same payload as the HTTP CreateComment path.
	resp := commentToResponse(comment, nil, nil)
	// source="lark_thread" is the loop-prevention contract the P5.A
	// outbound listener checks — keeping this in sync means an inbound
	// reply doesn't echo right back into the same thread.
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "member", uuidToString(userLink.UserID), map[string]any{
		"comment":             resp,
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"source":              "lark_thread",
	})

	// Wake the assigned agent's on_comment trigger so the agent reads
	// the answer and "continues working" per design §6.6 ("agent 在自己
	// 的 comment 流里看到回答, 继续干"). The CreateComment HTTP handler
	// applies the same gate — on_comment fires only for member-authored
	// comments on issues whose assignee is an active agent without a
	// pending task. We deliberately skip the mention-trigger fan-out
	// (handler.enqueueMentionedAgentTasks) because Lark message text
	// can't carry multica @-mentions anyway, and inheriting a thread
	// root's mentions doesn't apply when there's no multica thread root.
	if h.shouldEnqueueOnComment(ctx, issue) {
		if _, err := h.TaskService.EnqueueTaskForIssue(ctx, issue, comment.ID); err != nil {
			slog.Warn("lark inbound: enqueue on_comment failed",
				"err", err, "issue_id", uuidToString(issue.ID))
		}
	}
}
