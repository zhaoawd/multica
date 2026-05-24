package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Lark slash commands (§14.1.2): `/help`, `/status`, `/whoami`.
//
// Why this lives here and not in service/: the trio is hard-coded by
// design (no DSL, no NLU, no aliases). The parser is one switch over
// three exact strings; lifting it into a shared package would imply
// reuse this surface doesn't have.
//
// Why exact-match only: §11 invariant 4 ("no NLU"). A future "帮助" /
// "/help me with X" handler is explicitly out of scope — adding it
// belongs in a separate RFC, not a quiet extension to this parser.

type larkSlashCommand string

const (
	larkSlashNone   larkSlashCommand = ""
	larkSlashHelp   larkSlashCommand = "/help"
	larkSlashStatus larkSlashCommand = "/status"
	larkSlashWhoami larkSlashCommand = "/whoami"
)

// parseLarkSlashCommand returns one of the three commands, or "" if the
// text is not a recognised slash command. Trailing whitespace or args
// after the command word still match (e.g. "/help " or "/help me") —
// the commands ignore args and surface the same response either way.
// Anything that looks like a prefix but is actually a longer word
// ("/helpme") does NOT match — slash commands are word-boundaried.
func parseLarkSlashCommand(text string) larkSlashCommand {
	s := strings.TrimSpace(text)
	if s == "" || s[0] != '/' {
		return larkSlashNone
	}
	// Split on the first whitespace run. The head is the command word.
	head := s
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		head = s[:i]
	}
	switch larkSlashCommand(head) {
	case larkSlashHelp:
		return larkSlashHelp
	case larkSlashStatus:
		return larkSlashStatus
	case larkSlashWhoami:
		return larkSlashWhoami
	}
	return larkSlashNone
}

// handleLarkSlashCommand dispatches a parsed slash command. Called from
// handleLarkMessageEvent BEFORE the @bot verb parser — slash commands
// short-circuit the verb path even when the text would otherwise also
// be a verb (impossible in practice, but the precedence is explicit).
//
// Two-channel response model:
//   - In-channel reply: posted via ReplyToMessage so the bot's answer
//     threads under the user's command.
//   - DM fan-out: only `/whoami` uses this — the in-channel reply
//     stays neutral (no binding leak), and detailed info goes
//     privately if the sender has a lark_user_link.
//
// The slash trio runs WITHOUT requiring a workspace binding. /help and
// /whoami are discoverability surfaces — they must work even when the
// admin hasn't yet bound the chat. /status is the one that depends on
// a binding; it surfaces the missing-binding state explicitly.
func (h *Handler) handleLarkSlashCommand(
	ctx context.Context,
	msg larkInboundMessage,
	senderOpenID string,
	slash larkSlashCommand,
) {
	if h.LarkThread == nil || h.LarkThread.Client == nil {
		return
	}

	switch slash {
	case larkSlashHelp:
		h.replyToLarkMessage(ctx, msg.MessageID, larkSlashHelpText())

	case larkSlashStatus:
		h.replyToLarkMessage(ctx, msg.MessageID, h.larkSlashStatusText(ctx, msg))

	case larkSlashWhoami:
		h.handleLarkSlashWhoami(ctx, msg, senderOpenID)
	}
}

// larkSlashHelpText is the hard-coded `/help` body. Kept static so a
// future change to the available verbs forces an explicit code edit —
// no auto-generation off a registry.
//
// Per §14.1.2: list `@bot` verbs + HITL stages + a DM-prefs link.
// HITL is staged for §13/§14.1.1; we name the stages today so new
// users know what to expect, but flag them as "to land in HITL".
func larkSlashHelpText() string {
	frontend := larkFrontendOrigin()
	var b strings.Builder
	b.WriteString("Multica bot 指令:\n")
	b.WriteString("\n")
	b.WriteString("• @bot 创建任务 <内容> — 从当前 thread 创建 issue\n")
	b.WriteString("• @bot link-doc — 关联飞书文档 (暂未实装)\n")
	b.WriteString("• @bot open-meeting — 创建同步会议 (暂未实装)\n")
	b.WriteString("\n")
	b.WriteString("Slash 指令:\n")
	b.WriteString("• /help — 显示本帮助\n")
	b.WriteString("• /status — 当前群的 workspace 绑定与事件订阅\n")
	b.WriteString("• /whoami — 当前 Lark 账号的 Multica 绑定状态 (结果以 DM 投递)\n")
	b.WriteString("\n")
	b.WriteString("HITL 阶段 (随 §13 HITL 上线):\n")
	b.WriteString("• planning → developing → testing → done\n")
	b.WriteString("\n")
	b.WriteString("个人偏好设置: ")
	b.WriteString(frontend)
	b.WriteString("/settings/profile")
	return b.String()
}

// larkSlashStatusText renders the `/status` body. Per §14.1.2 it shows
// workspace-level group properties ONLY — no per-user prefs leak here.
//
// Order of disclosure:
//  1. Bot online / configured.
//  2. Workspace this chat is bound to.
//  3. Enabled events on that binding.
//
// Each is one short line. Missing data renders as "(none)" — the user
// still sees the field name, which is the diagnostic value.
func (h *Handler) larkSlashStatusText(ctx context.Context, msg larkInboundMessage) string {
	var b strings.Builder
	b.WriteString("Multica bot status:\n")

	configured := h.LarkThread != nil && h.LarkThread.Configured()
	b.WriteString(fmt.Sprintf("• Bot: %s\n", boolOnOff(configured)))

	// /status reads from the bound binding, regardless of the chat
	// type. For p2p chats there is no binding (DMs aren't bound to
	// workspaces), so we surface that as "(none)" rather than
	// pretending the user's linked workspaces would apply — that
	// would leak personal binding state into a workspace-property
	// surface, exactly what §14.1.2 forbids.
	binding, err := h.Queries.GetLarkWorkspaceBindingByChatID(ctx, msg.ChatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			b.WriteString("• Workspace: (this chat is not bound)\n")
			b.WriteString("• Enabled events: (n/a)")
			return b.String()
		}
		slog.Warn("lark /status: binding lookup", "err", err, "chat_id", msg.ChatID)
		b.WriteString("• Workspace: (lookup failed)\n")
		b.WriteString("• Enabled events: (lookup failed)")
		return b.String()
	}

	wsName := "(unknown)"
	wsSlug := ""
	if ws, err := h.Queries.GetWorkspace(ctx, binding.WorkspaceID); err == nil {
		wsName = ws.Name
		wsSlug = ws.Slug
	}
	if wsSlug != "" {
		b.WriteString(fmt.Sprintf("• Workspace: %s (%s)\n", wsName, wsSlug))
	} else {
		b.WriteString(fmt.Sprintf("• Workspace: %s\n", wsName))
	}

	if len(binding.EnabledEvents) == 0 {
		b.WriteString("• Enabled events: (none)")
	} else {
		b.WriteString("• Enabled events: ")
		b.WriteString(strings.Join(binding.EnabledEvents, ", "))
	}
	return b.String()
}

// handleLarkSlashWhoami implements the §14.1.2 privacy contract for
// `/whoami`:
//
//   - In a group chat: the in-chat reply is ALWAYS the same neutral
//     line, with no information about whether the sender is linked.
//     Different replies for linked vs unlinked users would leak that
//     state to bystanders.
//   - In a DM (chat_type=p2p): reply with the actual binding details
//     in-channel — the only viewer is the sender themselves.
//   - In both cases: if the sender is linked, also send a DM with
//     the details. The group user gets the answer in their inbox;
//     the DM user gets it twice, harmlessly.
func (h *Handler) handleLarkSlashWhoami(ctx context.Context, msg larkInboundMessage, senderOpenID string) {
	isDM := msg.ChatType == "p2p"

	// Resolve the sender. err==pgx.ErrNoRows means "not linked"; any
	// other error is treated as "lookup failed" — we still send the
	// neutral group reply so we don't leak via timing or absence.
	link, lookupErr := h.larkLookupUserLink(ctx, senderOpenID)

	body := larkWhoamiBodyText(ctx, h.Queries, link, lookupErr)

	if isDM {
		// DM: the reply itself is private. Just answer in place.
		h.replyToLarkMessage(ctx, msg.MessageID, body)
		return
	}

	// Group: in-chat answer is neutral, then DM the details (when
	// possible). Order matters — reply first so the user sees an
	// immediate ack, then fan out the DM asynchronously-equivalent
	// (sequential but small).
	h.replyToLarkMessage(ctx, msg.MessageID, larkWhoamiGroupNeutral)

	if lookupErr != nil || senderOpenID == "" {
		// Unlinked user (or DB hiccup): no DM to send. The neutral
		// group reply already pointed them at the settings page.
		return
	}
	if err := h.LarkThread.Client.SendTextMessage(ctx, "open_id", senderOpenID, body); err != nil {
		slog.Warn("lark /whoami: DM send failed", "err", err, "open_id", senderOpenID)
	}
}

// larkWhoamiGroupNeutral is the §14.1.2 mandated neutral group reply.
// CONSTANT — do not template per-user, do not branch on linked state.
// A "you are not linked, see X" variant looks helpful but tells every
// onlooker in the group whether that user is linked.
const larkWhoamiGroupNeutral = "已尝试通过 DM 发送结果；请在 Multica 个人设置确认绑定状态。"

// larkLookupUserLink returns the lark_user_link row for an open_id,
// or (zero, pgx.ErrNoRows) when the user is not linked. Other errors
// pass through so the caller can choose to log and still emit a
// neutral response.
func (h *Handler) larkLookupUserLink(ctx context.Context, openID string) (db.LarkUserLink, error) {
	if openID == "" {
		return db.LarkUserLink{}, pgx.ErrNoRows
	}
	return h.Queries.GetLarkUserLinkByOpenID(ctx, openID)
}

// larkWhoamiBodyText builds the DM-or-self body that names the binding
// state. Used both as the DM payload (for group sends) and as the
// in-channel reply (for DM sends). Same content, two surfaces.
//
// Sections:
//   - account binding (linked or not, with masked open_id when linked)
//   - DM notifications on/off
//   - workspaces the user is a member of (so they can confirm scope)
//
// The masked open_id helps the user confirm "this is the right
// account" without exposing the full identifier to copy/paste leaks
// (the value itself is not a credential, but it's identifying).
func larkWhoamiBodyText(ctx context.Context, q *db.Queries, link db.LarkUserLink, lookupErr error) string {
	var b strings.Builder
	b.WriteString("Multica 账号状态:\n")

	if lookupErr != nil {
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			b.WriteString("• 状态: 未绑定\n")
			b.WriteString("• 绑定入口: ")
			b.WriteString(larkFrontendOrigin())
			b.WriteString("/settings/profile")
			return b.String()
		}
		// Non-ErrNoRows lookup failure — still neutral.
		b.WriteString("• 状态: 查询失败，请稍后再试")
		return b.String()
	}

	b.WriteString("• 状态: 已绑定 (")
	b.WriteString(maskOpenID(link.LarkOpenID))
	b.WriteString(")\n")
	// DM-enabled flag (lark_user_link.dm_enabled) is in the design's
	// §5.2 schema but not yet in the actual migration — surfacing it
	// here would require either a new column or a fabricated default.
	// Either is out of scope for §14.1.2; we render only what the
	// database actually knows.

	// Workspace list. Empty = the user is linked but has been removed
	// from every workspace — possible in practice, show "(none)" so
	// they know the bot can't act for them anywhere.
	workspaces, err := q.ListWorkspaces(ctx, link.UserID)
	if err != nil {
		slog.Warn("lark /whoami: workspace list", "err", err, "user", uuidToString(link.UserID))
		b.WriteString("• Workspaces: (查询失败)")
		return b.String()
	}
	if len(workspaces) == 0 {
		b.WriteString("• Workspaces: (none)")
		return b.String()
	}
	b.WriteString("• Workspaces:\n")
	for _, ws := range workspaces {
		b.WriteString("    – ")
		b.WriteString(ws.Name)
		if ws.Slug != "" {
			b.WriteString(" (")
			b.WriteString(ws.Slug)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	// Trim trailing newline so the reply doesn't have a dangling blank line.
	return strings.TrimRight(b.String(), "\n")
}

// maskOpenID returns a fingerprint suitable for echoing back to the
// user — enough characters to recognise their own ID, with the middle
// hidden so a screenshot doesn't dump the whole value. Lark open IDs
// look like `ou_<27 hex>`; we keep the `ou_` prefix and the first /
// last few characters of the random tail.
func maskOpenID(openID string) string {
	if len(openID) <= 10 {
		return openID
	}
	return openID[:6] + "…" + openID[len(openID)-4:]
}

// boolOnOff renders a boolean as a short human label. Used by status
// and whoami text rendering. Kept here so the wording is consistent
// across both commands.
func boolOnOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// larkFrontendOrigin returns the configured frontend origin (without
// trailing slash) for inclusion in DM bodies and help text. Falls
// back to the dev default so the messages stay coherent in a
// developer's local environment.
func larkFrontendOrigin() string {
	frontend := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	return strings.TrimRight(frontend, "/")
}
