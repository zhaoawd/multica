package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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

	var payload LarkCardCallback
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}

	// URL verification handshake — Lark sends this once at setup.
	if payload.Type == "url_verification" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": payload.Challenge})
		return
	}

	// Action callback — only "button" tag is wired in P2.2.
	if payload.Action.Tag != "button" {
		// Other tags (select_static, picker, ...) may come from cards we
		// haven't built. Acknowledge silently so Lark doesn't retry.
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
	ctx := r.Context()

	verb, _ := payload.Action.Value["verb"].(string)
	issueIDStr, _ := payload.Action.Value["issue_id"].(string)
	if verb == "" || issueIDStr == "" {
		writeJSON(w, http.StatusOK, toastResponse("error", "missing verb or issue_id"))
		return
	}

	// 1) Resolve clicker → multica user via lark_user_link. An unlinked
	//    Lark user gets a guiding message rather than a silent failure;
	//    they typically just need to visit Settings → Profile and
	//    connect their account once.
	if payload.OpenID == "" {
		writeJSON(w, http.StatusOK, toastResponse("error", "missing open_id"))
		return
	}
	link, err := h.Queries.GetLarkUserLinkByOpenID(ctx, payload.OpenID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, toastResponse("warning",
				"Link your Lark account in Multica → Settings → Profile to act on cards."))
			return
		}
		slog.Warn("lark webhook: lookup user link", "err", err, "open_id", payload.OpenID)
		writeJSON(w, http.StatusOK, toastResponse("error", "internal error"))
		return
	}

	// 2) Load the issue. The IssueID lives in action.value, which was
	//    written by us at card-build time, but we still re-validate at
	//    the DB boundary: a stale card might reference an issue that's
	//    been deleted, or a malicious Lark user could craft a button
	//    payload by hand.
	issueUUID, err := parseStrictUUID(issueIDStr)
	if err != nil {
		writeJSON(w, http.StatusOK, toastResponse("error", "bad issue id"))
		return
	}
	issue, err := h.Queries.GetIssue(ctx, issueUUID)
	if err != nil {
		writeJSON(w, http.StatusOK, toastResponse("error", "issue not found"))
		return
	}

	// 3) Verify the clicker is a workspace member of the issue. Without
	//    this, anyone with the Lark bot in their chat could mutate
	//    issues in workspaces they don't belong to.
	if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{
		UserID:      link.UserID,
		WorkspaceID: issue.WorkspaceID,
	}); err != nil {
		writeJSON(w, http.StatusOK, toastResponse("warning",
			"You are not a member of this workspace."))
		return
	}

	switch verb {
	case "claim":
		h.larkClaimIssue(ctx, w, issue, link.UserID)
	case "mark_done":
		h.larkMarkIssueDone(ctx, w, issue)
	default:
		writeJSON(w, http.StatusOK, toastResponse("error", "unsupported action"))
	}
}

// larkClaimIssue assigns the issue to the clicking user. The "member"
// assignee_type matches what the issue-detail UI uses when a human
// claims via the multica web client.
func (h *Handler) larkClaimIssue(ctx context.Context, w http.ResponseWriter, issue db.Issue, userID pgtype.UUID) {
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
		writeJSON(w, http.StatusOK, toastResponse("error", "failed to claim"))
		return
	}
	wsID := uuidToString(issue.WorkspaceID)
	prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
	resp := issueToResponse(updated, prefix)
	h.publish(protocol.EventIssueUpdated, wsID, "member", uuidToString(userID), map[string]any{
		"issue":             resp,
		"assignee_changed":  true,
		"prev_assignee_id":  uuidToString(issue.AssigneeID),
		"source":            "lark_card_action",
	})
	writeJSON(w, http.StatusOK, toastResponse("success", "Claimed"))
}

// larkMarkIssueDone moves the issue to "done". Already-done issues short
// the publish but still report success — the user clicked Mark Done and
// the desired end state is already reached.
func (h *Handler) larkMarkIssueDone(ctx context.Context, w http.ResponseWriter, issue db.Issue) {
	if issue.Status == "done" {
		writeJSON(w, http.StatusOK, toastResponse("info", "Already done"))
		return
	}
	updated, err := h.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID:     issue.ID,
		Status: "done",
	})
	if err != nil {
		slog.Warn("lark webhook: mark done failed", "err", err, "issue_id", uuidToString(issue.ID))
		writeJSON(w, http.StatusOK, toastResponse("error", "failed to mark done"))
		return
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
	writeJSON(w, http.StatusOK, toastResponse("success", "Marked done"))
}
