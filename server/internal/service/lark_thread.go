// Package service — Lark thread ↔ issue bridge (Phase 4).
//
// This file handles the Lark side of the @bot 创建任务 verb:
//   1. Parse the @bot message text into a structured verb (or none).
//   2. When the verb is "create_issue", fetch surrounding thread context
//      via the Lark messages list API.
//   3. Wrap counter-increment + issue insert + lark_issue_link insert in
//      a single transaction so a partial failure can't leave a bridged
//      issue without its link row.
//   4. Reply into the originating Lark thread with "已创建 multica-<n>".
//
// What this file deliberately does NOT do (LARK_INTEGRATION_DESIGN.md
// §6.5 / §10):
//   - No NLU. The verb whitelist is hard-coded and case-sensitive.
//     Unknown verbs fall through silently — the goal is "avoid misfires
//     when humans chat naturally in a thread the bot also lives in".
//   - No assignment. P4 creates the issue unassigned with status=todo;
//     the human triages it from multica or from another @bot verb.
//   - No agent dispatch. EnqueueTaskForIssue is intentionally NOT
//     called — an unassigned issue has nothing to dispatch to, and the
//     "auto-assign on creation" semantics live one level up at the
//     issue creation handler, not in the lark bridge.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// LarkBotVerb enumerates the structured commands the @bot understands.
// Per design §6.5 only `create_issue` is implemented in P4; the other
// two are reserved (parser recognises them so we can ship "not yet"
// replies rather than silent ignores when the user is clearly trying).
type LarkBotVerb string

const (
	LarkVerbNone        LarkBotVerb = ""
	LarkVerbCreateIssue LarkBotVerb = "create_issue"
	LarkVerbLinkDoc     LarkBotVerb = "link_doc"     // reserved for a later phase
	LarkVerbOpenMeeting LarkBotVerb = "open_meeting" // reserved for a later phase

	// LarkIssueTitleMaxRunes caps how many runes from the trigger
	// message body become the issue title. The rest goes into the
	// description. Matches the soft limit the multica UI shows for
	// single-line titles without truncation styling.
	LarkIssueTitleMaxRunes = 80
)

// larkVerbAliases maps the literal text the user typed (after the @bot
// mention is stripped) to a canonical verb. Order matters: longer
// aliases must come first so "创建任务" wins over a future "创建" prefix.
var larkVerbAliases = []struct {
	prefix string
	verb   LarkBotVerb
}{
	{"创建任务", LarkVerbCreateIssue},
	{"create_issue", LarkVerbCreateIssue},
	{"create-issue", LarkVerbCreateIssue},
	{"link-doc", LarkVerbLinkDoc},
	{"link_doc", LarkVerbLinkDoc},
	{"open-meeting", LarkVerbOpenMeeting},
	{"open_meeting", LarkVerbOpenMeeting},
}

// ParseLarkBotVerb scans text (the @bot message body with the mention
// placeholder already stripped) for a leading structured verb.
//
// Returns (verb, remainder) where remainder is the trailing text the
// user wants captured as the issue body. When no recognised verb leads,
// returns (LarkVerbNone, "") so callers can treat "unknown verb" the
// same as "no verb at all" — silently ignored, per the no-NLU rule.
func ParseLarkBotVerb(text string) (LarkBotVerb, string) {
	t := strings.TrimSpace(text)
	if t == "" {
		return LarkVerbNone, ""
	}
	for _, alias := range larkVerbAliases {
		if !strings.HasPrefix(t, alias.prefix) {
			continue
		}
		rest := strings.TrimSpace(t[len(alias.prefix):])
		// Defend against a verb-shaped prefix that's actually part of a
		// longer word: require either end-of-string or a whitespace
		// boundary after the verb token. Without this, "创建任务模板更新了"
		// would parse as "create_issue" with body "模板更新了".
		if rest == "" {
			return alias.verb, ""
		}
		if !isLarkVerbBoundary(t, len(alias.prefix)) {
			continue
		}
		return alias.verb, rest
	}
	return LarkVerbNone, ""
}

// isLarkVerbBoundary returns true when the rune immediately after the
// verb prefix is whitespace (space, tab, newline, full-width space).
// Chinese verbs like 创建任务 don't have a visible space delimiter, so
// we don't require one for them — but we DO require the prefix to end
// the original token cleanly, which `strings.HasPrefix` already
// guarantees relative to the cursor we pass.
//
// The implementation is dumb on purpose: the alias set is tiny and the
// caller already trimmed leading whitespace, so a single peek is
// enough.
func isLarkVerbBoundary(s string, cursor int) bool {
	if cursor >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[cursor:])
	switch r {
	case ' ', '\t', '\n', '\r', '　':
		return true
	}
	return false
}

// StripLarkMentionPlaceholders rewrites a Lark text-message body to
// remove the `@_user_N` placeholders Lark inserts where @-mentions
// appear. The webhook handler already knows which placeholder is the
// bot — for issue body purposes we drop all of them so the description
// reads as plain text without `@_user_1 @_user_2` noise.
//
// Lark's placeholder format is fixed: literal `@_user_` followed by one
// or more decimal digits. We replace each with the empty string and
// collapse runs of whitespace the removal leaves behind.
func StripLarkMentionPlaceholders(text string) string {
	// Walk char-by-char rather than reaching for regexp — the format is
	// trivial enough that the regex compile dominates the win.
	var b strings.Builder
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "@_user_") {
			j := i + len("@_user_")
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			if j > i+len("@_user_") {
				i = j
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	// Collapse double spaces that the removal left behind.
	out := strings.TrimSpace(b.String())
	for strings.Contains(out, "  ") {
		out = strings.ReplaceAll(out, "  ", " ")
	}
	return out
}

// LarkThreadContext captures everything the @bot 创建任务 verb needs to
// build a multica issue. It is the output of FetchThreadContext and the
// input to CreateIssueFromThread.
//
// RootMessageID is the open_message_id we store in lark_issue_link. It
// is the message that "anchors" the bridge — replies into the
// originating Lark thread (P5) will attach to this id via Lark's reply
// endpoint. When the @bot mention is itself the root (no enclosing
// thread), RootMessageID equals TriggerMessageID.
type LarkThreadContext struct {
	ChatID           string
	ThreadID         string // empty when @bot was in a top-level message, not inside a thread
	TriggerMessageID string
	RootMessageID    string
	Body             string // remainder text after the verb prefix (user's own description)
	ThreadMessages   []LarkThreadMessage
}

// LarkThreadService bridges Lark @bot mentions into multica issues.
//
// Construction is intentionally lightweight: no internal state, no
// background goroutines. One instance lives on the Handler and the
// webhook calls into it.
type LarkThreadService struct {
	Queries   *db.Queries
	TxStarter TxStarter
	Bus       *events.Bus
	Client    *LarkClient
}

// NewLarkThreadService wires the service. Pass `client == nil` when
// Lark is unconfigured — the methods then return an "unavailable"
// error so the webhook can reply with a clear message instead of
// crashing.
func NewLarkThreadService(q *db.Queries, tx TxStarter, bus *events.Bus, client *LarkClient) *LarkThreadService {
	return &LarkThreadService{Queries: q, TxStarter: tx, Bus: bus, Client: client}
}

// Configured reports whether the underlying LarkClient has full env.
// The webhook checks this before calling FetchThreadContext / Reply so
// it can route the unconfigured case to a single "integration off"
// reply rather than letting two methods race the same error code.
func (s *LarkThreadService) Configured() bool {
	return s != nil && s.Client != nil && s.Client.cfg.Configured()
}

// FetchThreadContext pulls thread messages from Lark when threadID is
// non-empty, and returns a LarkThreadContext suitable for
// CreateIssueFromThread.
//
// When threadID is empty (the @bot was mentioned in a top-level chat
// message, not inside a thread), the result still describes a valid
// "single message" context — RootMessageID = TriggerMessageID and
// ThreadMessages is nil.
//
// Errors from the Lark list API are NOT fatal: they are logged and the
// context returns with ThreadMessages=nil. The caller still gets a
// usable context — the issue body just falls back to the user's own
// trigger text instead of inheriting the thread transcript.
func (s *LarkThreadService) FetchThreadContext(ctx context.Context, chatID, triggerMsgID, threadID, body string) *LarkThreadContext {
	tc := &LarkThreadContext{
		ChatID:           chatID,
		ThreadID:         threadID,
		TriggerMessageID: triggerMsgID,
		RootMessageID:    triggerMsgID,
		Body:             strings.TrimSpace(body),
	}
	if threadID == "" || s.Client == nil || !s.Client.cfg.Configured() {
		return tc
	}
	msgs, err := s.Client.ListThreadMessages(ctx, threadID, LarkMaxThreadFetchMessages)
	if err != nil {
		slog.Warn("lark thread: list messages failed", "err", err, "thread_id", threadID)
		return tc
	}
	tc.ThreadMessages = msgs
	if len(msgs) > 0 && msgs[0].MessageID != "" {
		// Anchor the bridge on the actual thread root rather than the
		// @bot mention message. P5's inbound bridge replies into this
		// root, which keeps follow-ups grouped under the original
		// discussion regardless of which message triggered the bot.
		tc.RootMessageID = msgs[0].MessageID
	}
	return tc
}

// titleAndDescription splits a LarkThreadContext into the issue title
// and description.
//
// Title rules:
//   - First non-empty single line of the user's trigger body.
//   - If absent, the first non-empty line of the earliest thread
//     message we fetched.
//   - If still absent, a generic "Lark thread issue" placeholder so
//     CreateIssue doesn't reject on the empty-title path.
//   - Capped at LarkIssueTitleMaxRunes; remainder folds into the body.
//
// Description rules:
//   - Trigger body comes first (when present), labelled with the
//     command author.
//   - Then a transcript of the fetched thread (oldest → newest),
//     skipping empty messages. Each line is prefixed with
//     "[<HH:MM>] <open_id_suffix>: ".
//   - Final paragraph credits the bridge so a human reading the issue
//     description in multica understands the origin.
func (tc *LarkThreadContext) titleAndDescription() (title, description string) {
	body := strings.TrimSpace(tc.Body)
	if body != "" {
		title, body = splitLeadingLine(body)
	}
	if title == "" && len(tc.ThreadMessages) > 0 {
		// fall back to the earliest thread message we fetched
		for _, m := range tc.ThreadMessages {
			cand := StripLarkMentionPlaceholders(m.Text)
			if cand == "" {
				continue
			}
			title, _ = splitLeadingLine(cand)
			break
		}
	}
	if title == "" {
		title = "Lark thread issue"
	}
	title = truncateRunes(title, LarkIssueTitleMaxRunes)

	var b strings.Builder
	if body != "" {
		b.WriteString(strings.TrimSpace(body))
	}
	if len(tc.ThreadMessages) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("---\n")
		b.WriteString("Lark thread transcript:\n")
		for _, m := range tc.ThreadMessages {
			text := StripLarkMentionPlaceholders(m.Text)
			if text == "" {
				continue
			}
			ts := m.CreatedAt.Format("15:04")
			b.WriteString(fmt.Sprintf("[%s] %s: %s\n", ts, shortenOpenID(m.SenderOpenID), text))
		}
	}
	return title, strings.TrimSpace(b.String())
}

// CreateIssueFromThread inserts an issue + lark_issue_link row in one
// transaction and publishes the EventIssueCreated event so the existing
// activity / notification chain fires. Returns the created issue.
//
// The creator is the multica user resolved from the sender's
// lark_open_id (the webhook does the lookup before calling). When the
// sender isn't linked, the webhook short-circuits with a "link your
// account" reply and never calls this method.
func (s *LarkThreadService) CreateIssueFromThread(
	ctx context.Context,
	workspaceID pgtype.UUID,
	creator pgtype.UUID,
	tc *LarkThreadContext,
) (db.Issue, error) {
	if s == nil {
		return db.Issue{}, errors.New("lark thread service nil")
	}
	if !workspaceID.Valid || !creator.Valid {
		return db.Issue{}, errors.New("workspace_id and creator required")
	}
	if tc == nil || tc.RootMessageID == "" || tc.ChatID == "" {
		return db.Issue{}, errors.New("thread context missing chat_id or root_message_id")
	}

	title, description := tc.titleAndDescription()

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.Issue{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)

	issueNumber, err := qtx.IncrementIssueCounter(ctx, workspaceID)
	if err != nil {
		return db.Issue{}, fmt.Errorf("increment issue counter: %w", err)
	}

	issue, err := qtx.CreateIssue(ctx, db.CreateIssueParams{
		WorkspaceID:   workspaceID,
		Title:         title,
		Description:   pgtype.Text{String: description, Valid: description != ""},
		Status:        "todo",
		Priority:      "none",
		AssigneeType:  pgtype.Text{},
		AssigneeID:    pgtype.UUID{},
		CreatorType:   "member",
		CreatorID:     creator,
		ParentIssueID: pgtype.UUID{},
		Position:      0,
		StartDate:     pgtype.Timestamptz{},
		DueDate:       pgtype.Timestamptz{},
		Number:        issueNumber,
		ProjectID:     pgtype.UUID{},
	})
	if err != nil {
		return db.Issue{}, fmt.Errorf("create issue: %w", err)
	}

	if _, err := qtx.InsertLarkIssueLink(ctx, db.InsertLarkIssueLinkParams{
		IssueID:       issue.ID,
		ChatID:        tc.ChatID,
		RootMessageID: tc.RootMessageID,
	}); err != nil {
		return db.Issue{}, fmt.Errorf("insert lark issue link: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return db.Issue{}, fmt.Errorf("commit tx: %w", err)
	}

	prefix := s.getIssuePrefix(ctx, workspaceID)
	s.Bus.Publish(events.Event{
		Type:        protocol.EventIssueCreated,
		WorkspaceID: util.UUIDToString(workspaceID),
		ActorType:   "member",
		ActorID:     util.UUIDToString(creator),
		Payload: map[string]any{
			"issue":  issueToMap(issue, prefix),
			"source": "lark_thread",
		},
	})
	return issue, nil
}

// ReplyWithIssueCreated posts "已创建 multica-NN — <title>" back into
// the originating thread. Falls back to a no-op (logged) when Lark
// rejects the reply — the issue is already created in multica, so
// failure of the cosmetic confirmation must not be treated as a
// dispatch failure.
func (s *LarkThreadService) ReplyWithIssueCreated(ctx context.Context, tc *LarkThreadContext, issueIdentifier, title string) {
	if s == nil || s.Client == nil || tc == nil || tc.TriggerMessageID == "" {
		return
	}
	body := fmt.Sprintf("已创建 %s — %s", issueIdentifier, title)
	if err := s.Client.ReplyToMessage(ctx, tc.TriggerMessageID, body); err != nil {
		slog.Warn("lark thread: reply failed",
			"err", err,
			"trigger_message_id", tc.TriggerMessageID,
			"issue", issueIdentifier,
		)
	}
}

// getIssuePrefix mirrors AutopilotService.getIssuePrefix so the
// EventIssueCreated payload contains a renderable identifier. Errors
// degrade to "" rather than blocking the publish — the prefix is
// presentational, the event still fires correctly without it.
func (s *LarkThreadService) getIssuePrefix(ctx context.Context, workspaceID pgtype.UUID) string {
	ws, err := s.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return ""
	}
	return ws.IssuePrefix
}

// ── small string utilities ─────────────────────────────────────────────

func splitLeadingLine(s string) (first, rest string) {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:])
	}
	return strings.TrimSpace(s), ""
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

// shortenOpenID returns a short identifier for thread-transcript
// rendering. Lark open_ids look like `ou_8c1f...` (28+ chars); printing
// the full id makes the description unreadable. We keep the prefix
// suffix so collisions in a single thread remain distinguishable.
func shortenOpenID(openID string) string {
	if openID == "" {
		return "unknown"
	}
	if utf8.RuneCountInString(openID) <= 12 {
		return openID
	}
	runes := []rune(openID)
	return string(runes[:8]) + "…"
}

