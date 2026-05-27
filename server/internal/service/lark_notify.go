package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// LarkNotify is the outbound notifier — it renders an interactive card per
// event kind and POSTs it to the workspace's bound chat. The set of event
// kinds we render is hard-coded here (no DSL) per LARK_INTEGRATION_DESIGN.md
// §6.1; per-workspace enable/disable is enforced via
// lark_workspace_binding.enabled_events.
//
// We use multica's own protocol.EventXxx constants as the enabled_events
// vocabulary so the front-end checklist, the DB column, and the event bus
// all speak the same identifiers — no translation table to drift.
//
// Concurrency model: dispatch() is called on the synchronous event bus
// goroutine (whichever request thread emitted the issue/comment/task
// event). We MUST NOT do the HTTP call out to Lark on that thread — a
// slow or unreachable Lark endpoint would stall every write that publishes
// to the bus. So dispatch only does the cheap, local work (binding lookup
// + gating + card render) inline, then drops the actual send onto a
// bounded worker queue. If the queue is full (Lark outage backing up
// past our buffer) we drop the event with a WARN — better to lose a card
// than to back-pressure the issue write path.
type LarkNotify struct {
	cfg      LarkConfig
	client   *LarkClient
	queries  *db.Queries
	frontend string
	log      *slog.Logger

	jobs        chan larkJob
	wg          sync.WaitGroup
	started     atomic.Bool
	stopping    atomic.Bool   // set in Stop; gates new enqueues
	stopCh      chan struct{} // closed in Stop; workers select-exit on it
	stopOnce    sync.Once
	sendTimeout time.Duration
}

// larkSendMode distinguishes team-chat sends from personal DM sends.
type larkSendMode string

const (
	larkSendChat larkSendMode = "chat"
	larkSendDM   larkSendMode = "dm"
)

// larkJob is what the bus goroutine enqueues for workers to dispatch.
// We capture the rendered card + target, not a closure over the binding
// row, so the worker doesn't pin queries / context past dispatch().
type larkJob struct {
	targetID string       // chat_id for team sends, open_id for DM sends
	sendMode larkSendMode // which LarkClient method to call
	card     any
	event    string
	wsID     string
}

const (
	larkJobBufferSize = 256              // ~5s headroom at 50 events/sec
	larkWorkerCount   = 2                // serializing isn't important; 2 covers headroom
	larkSendTimeout   = 15 * time.Second // bounded per HTTP attempt
)

// NewLarkNotify constructs a notifier. The returned value is always safe to
// call; if the integration is not configured every Notify* method is a
// no-op, so callers do not need to guard each invocation.
//
// Workers are NOT started here — call Start() once after construction so
// tests can drive dispatch synchronously by skipping Start.
func NewLarkNotify(queries *db.Queries) *LarkNotify {
	cfg := LarkConfigFromEnv()
	frontend := strings.TrimRight(strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN")), "/")
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	return &LarkNotify{
		cfg:         cfg,
		client:      NewLarkClient(cfg),
		queries:     queries,
		frontend:    frontend,
		log:         slog.Default(),
		jobs:        make(chan larkJob, larkJobBufferSize),
		stopCh:      make(chan struct{}),
		sendTimeout: larkSendTimeout,
	}
}

// Start launches the worker pool. Idempotent; no-op when the integration
// is unconfigured. Call this once at server startup.
func (n *LarkNotify) Start() {
	if !n.cfg.Configured() {
		return
	}
	if !n.started.CompareAndSwap(false, true) {
		return
	}
	for i := 0; i < larkWorkerCount; i++ {
		n.wg.Add(1)
		go n.runWorker()
	}
}

// Stop signals workers to exit and waits up to ctx's deadline. Pending
// jobs in the channel are abandoned — Lark cards are best-effort, and a
// long shutdown would block process exit during a Lark outage (workers
// busy stuck on the per-send timeout). We deliberately do NOT close the
// jobs channel: dispatch() runs from bus goroutines that may not have
// observed Stop yet, and sending on a closed channel would panic. The
// stopping flag prevents new enqueues; existing entries get GC'd with
// the channel after the process exits.
func (n *LarkNotify) Stop(ctx context.Context) {
	if !n.started.Load() {
		return
	}
	n.stopping.Store(true)
	n.stopOnce.Do(func() { close(n.stopCh) })

	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		n.log.Warn("lark: workers did not exit before shutdown deadline; abandoning in-flight sends")
	}
}

// runWorker pulls jobs until stopCh is closed. We select with stopCh as
// a peer so an idle worker (channel empty during a Lark outage backed
// up upstream) exits promptly on Stop instead of blocking the receive.
//
// Note: an in-flight send is NOT cancelled when stopCh closes — its
// per-send timeout (sendTimeout) bounds the worst case, and Stop's
// own deadline lets the main shutdown path move on if needed.
func (n *LarkNotify) runWorker() {
	defer n.wg.Done()
	for {
		select {
		case <-n.stopCh:
			return
		case job := <-n.jobs:
			ctx, cancel := context.WithTimeout(context.Background(), n.sendTimeout)
			var err error
			switch job.sendMode {
			case larkSendDM:
				err = n.client.SendInteractiveCardToUser(ctx, job.targetID, job.card)
			default:
				err = n.client.SendInteractiveCard(ctx, job.targetID, job.card)
			}
			if err != nil {
				n.log.Warn("lark: send failed", "event", job.event, "ws", job.wsID, "mode", job.sendMode, "err", err)
			}
			cancel()
		}
	}
}

// Configured reports whether the Lark integration env is fully populated.
// The settings UI gates the "Connect" / event toggles on this.
func (n *LarkNotify) Configured() bool { return n.cfg.Configured() }

// IssueInfo is the minimal set of fields a card needs. We do NOT take a
// handler.IssueResponse — that would invert the handler→service import
// direction. Listener-side code (cmd/server) projects the response into
// this shape before calling us.
//
// IssueID / WorkspaceID are UUIDs (string-encoded). They feed the action
// button payload so the webhook handler can route the click back to the
// right issue without re-doing slug→id resolution.
type IssueInfo struct {
	IssueID       string
	WorkspaceID   string
	Identifier    string
	Title         string
	WorkspaceSlug string
}

// IssueURL renders a deep-link to the issue. Falls back to the workspace
// home when slug or identifier are missing — better a partial link than
// none, since the card recipient may still get useful context.
func (n *LarkNotify) IssueURL(info IssueInfo) string {
	if info.WorkspaceSlug == "" {
		return n.frontend
	}
	if info.Identifier == "" {
		return fmt.Sprintf("%s/%s/issues", n.frontend, info.WorkspaceSlug)
	}
	return fmt.Sprintf("%s/%s/issues/%s", n.frontend, info.WorkspaceSlug, info.Identifier)
}

// NotifyIssueCreated emits an "issue created" card. Routing: unassigned
// issues go to the team chat (claim card); assigned issues go to the
// assignee's DM (falls back to team chat if the user hasn't linked Lark).
// Thread-linked issues route to thread_reply (handled by LarkThreadService).
func (n *LarkNotify) NotifyIssueCreated(ctx context.Context, workspaceID string, info IssueInfo, hasAssignee bool, assigneeUserID string, hasLarkIssueLink bool, creatorName string) {
	cond := LarkRoutingConditions{
		Event:            protocol.EventIssueCreated,
		HasAssignee:      hasAssignee,
		HasLarkIssueLink: hasLarkIssueLink,
	}
	n.dispatchRouted(ctx, workspaceID, protocol.EventIssueCreated, assigneeUserID, cond, func(ch LarkChannel) any {
		if ch == LarkChannelDM && hasAssignee {
			return n.buildIssueAssignedCard(info, "")
		}
		return n.buildIssueCreatedCard(info, hasAssignee, creatorName)
	})
}

func (n *LarkNotify) buildIssueCreatedCard(info IssueInfo, hasAssignee bool, creatorName string) map[string]any {
	title := fmt.Sprintf("📝 New issue: %s", info.Identifier)
	desc := info.Title
	if creatorName != "" {
		desc = fmt.Sprintf("%s\n\n_Created by %s_", desc, creatorName)
	}
	buttons := []cardButton{}
	if !hasAssignee && info.IssueID != "" && !n.cfg.IsWebSocket() {
		buttons = append(buttons, cardButton{
			Text:  "Claim",
			Type:  "primary",
			Value: map[string]any{"verb": "claim", "issue_id": info.IssueID},
		})
	}
	buttons = append(buttons, cardButton{Text: "View", URL: n.IssueURL(info)})
	return buildCard(title, desc, buttons)
}

// NotifyIssueAssigned emits an "issue assigned" card on assignee change.
// Routing: always goes to the assignee's DM (falls back to team chat if
// the user hasn't linked Lark).
func (n *LarkNotify) NotifyIssueAssigned(ctx context.Context, workspaceID string, info IssueInfo, assigneeName string, assigneeUserID string) {
	cond := LarkRoutingConditions{
		Event:           protocol.EventIssueUpdated,
		HasAssignee:     true,
		AssigneeChanged: true,
	}
	n.dispatchRouted(ctx, workspaceID, protocol.EventIssueUpdated, assigneeUserID, cond, func(_ LarkChannel) any {
		return n.buildIssueAssignedCard(info, assigneeName)
	})
}

func (n *LarkNotify) buildIssueAssignedCard(info IssueInfo, assigneeName string) map[string]any {
	title := fmt.Sprintf("👤 Assigned: %s", info.Identifier)
	desc := info.Title
	if assigneeName != "" {
		desc = fmt.Sprintf("%s\n\n_Assignee: %s_", desc, assigneeName)
	}
	buttons := []cardButton{}
	if info.IssueID != "" && !n.cfg.IsWebSocket() {
		buttons = append(buttons, cardButton{
			Text:  "Mark Done",
			Type:  "primary",
			Value: map[string]any{"verb": "mark_done", "issue_id": info.IssueID},
		})
	}
	buttons = append(buttons, cardButton{Text: "View", URL: n.IssueURL(info)})
	return buildCard(title, desc, buttons)
}

// NotifyTaskCompleted emits a "task completed" card. P1 doesn't yet link to
// PRs (P3); we just show the issue with a view button.
func (n *LarkNotify) NotifyTaskCompleted(ctx context.Context, workspaceID string, info IssueInfo) {
	n.dispatch(ctx, workspaceID, protocol.EventTaskCompleted, func() any {
		title := fmt.Sprintf("✅ Task done: %s", info.Identifier)
		return buildCard(title, info.Title, []cardButton{{Text: "View", URL: n.IssueURL(info)}})
	})
}

// NotifyTaskFailed emits a "task failed" card with the error summary.
// Card buttons in P1 are URL-only (no action.value) — Retry / Triage map
// to opening the issue page; P2 wires the actual cardbacks.
func (n *LarkNotify) NotifyTaskFailed(ctx context.Context, workspaceID string, info IssueInfo, errSummary string) {
	n.dispatch(ctx, workspaceID, protocol.EventTaskFailed, func() any {
		title := fmt.Sprintf("❌ Task failed: %s", info.Identifier)
		desc := info.Title
		if errSummary != "" {
			desc = fmt.Sprintf("%s\n\n```\n%s\n```", desc, truncate(errSummary, 400))
		}
		return buildCard(title, desc, []cardButton{{Text: "Open", URL: n.IssueURL(info)}})
	})
}

// NotifyComment emits a "comment posted" card. Listener side decides
// whether this comment merits a card (e.g. only @mentions) — the service
// just renders & dispatches.
func (n *LarkNotify) NotifyComment(ctx context.Context, workspaceID string, info IssueInfo, authorName, excerpt string) {
	n.dispatch(ctx, workspaceID, protocol.EventCommentCreated, func() any {
		title := fmt.Sprintf("💬 Comment on %s", info.Identifier)
		body := truncate(excerpt, 400)
		if authorName != "" {
			body = fmt.Sprintf("**%s:** %s", authorName, body)
		}
		return buildCard(title, body, []cardButton{{Text: "Reply", URL: n.IssueURL(info)}})
	})
}

// dispatch is the shared path: look up the workspace's binding, gate on
// enabled_events, render the card, and enqueue the actual HTTP send onto
// the worker pool. dispatch ITSELF stays on the bus goroutine so the
// binding lookup observes the same DB pool as the write that produced
// the event — fine because the lookup is a single PK-indexed query.
// The Lark HTTP call (slow, can hang) happens in runWorker.
//
// When the worker queue is full we drop the event with a WARN: a Lark
// outage shouldn't back-pressure issue / comment / task writes.
func (n *LarkNotify) dispatch(ctx context.Context, workspaceID string, eventKind string, build func() any) {
	if !n.cfg.Configured() {
		return
	}
	if n.stopping.Load() {
		return
	}
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return
	}
	binding, err := n.queries.GetLarkWorkspaceBinding(ctx, wsUUID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			n.log.Warn("lark: binding lookup failed", "ws", workspaceID, "err", err)
		}
		return
	}
	if binding.ChatID == "" || !sliceContains(binding.EnabledEvents, eventKind) {
		return
	}
	card := build()
	job := larkJob{targetID: binding.ChatID, sendMode: larkSendChat, card: card, event: eventKind, wsID: workspaceID}
	n.enqueue(ctx, job)
}

// dispatchRouted uses RouteLarkEvent to decide where to send the card.
// For DM sends it looks up the assignee's lark_open_id; if the user
// hasn't linked their Lark account, falls back to team chat.
// The build function receives the target channel so callers can select
// the appropriate card template per destination.
func (n *LarkNotify) dispatchRouted(ctx context.Context, workspaceID string, eventKind string, assigneeUserID string, cond LarkRoutingConditions, build func(LarkChannel) any) {
	if !n.cfg.Configured() {
		return
	}
	if n.stopping.Load() {
		return
	}
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return
	}
	binding, err := n.queries.GetLarkWorkspaceBinding(ctx, wsUUID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			n.log.Warn("lark: binding lookup failed", "ws", workspaceID, "err", err)
		}
		return
	}
	if binding.ChatID == "" || !sliceContains(binding.EnabledEvents, eventKind) {
		return
	}

	decision := RouteLarkEvent(cond)
	if len(decision.Channels) == 0 {
		return
	}

	for _, ch := range decision.Channels {
		card := build(ch)
		switch ch {
		case LarkChannelTeam:
			n.enqueue(ctx, larkJob{targetID: binding.ChatID, sendMode: larkSendChat, card: card, event: eventKind, wsID: workspaceID})

		case LarkChannelDM:
			openID := n.resolveAssigneeLarkOpenID(ctx, assigneeUserID)
			if openID == "" {
				n.enqueue(ctx, larkJob{targetID: binding.ChatID, sendMode: larkSendChat, card: card, event: eventKind, wsID: workspaceID})
			} else {
				n.enqueue(ctx, larkJob{targetID: openID, sendMode: larkSendDM, card: card, event: eventKind, wsID: workspaceID})
			}

		case LarkChannelThreadReply:
			// Thread replies are handled by LarkThreadService separately.
		}
	}
}

// resolveAssigneeLarkOpenID looks up the Lark open_id for a multica user UUID.
// Returns "" if the user hasn't linked or the ID is invalid.
func (n *LarkNotify) resolveAssigneeLarkOpenID(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	uuid, err := util.ParseUUID(userID)
	if err != nil {
		return ""
	}
	link, err := n.queries.GetLarkUserLink(ctx, uuid)
	if err != nil {
		return ""
	}
	return link.LarkOpenID
}

// enqueue puts a job onto the worker channel. When workers haven't started
// (tests, unconfigured) it sends inline. When the queue is full it drops.
func (n *LarkNotify) enqueue(ctx context.Context, job larkJob) {
	if !n.started.Load() {
		sendCtx, cancel := context.WithTimeout(ctx, n.sendTimeout)
		defer cancel()
		var err error
		switch job.sendMode {
		case larkSendDM:
			err = n.client.SendInteractiveCardToUser(sendCtx, job.targetID, job.card)
		default:
			err = n.client.SendInteractiveCard(sendCtx, job.targetID, job.card)
		}
		if err != nil {
			n.log.Warn("lark: send failed", "event", job.event, "ws", job.wsID, "mode", job.sendMode, "err", err)
		}
		return
	}
	select {
	case n.jobs <- job:
	default:
		n.log.Warn("lark: queue full, dropping event", "event", job.event, "ws", job.wsID)
	}
}

// ResolveWorkspaceSlug is exposed so listener code can fill IssueInfo
// without re-implementing the workspace lookup. Returns "" on miss.
func (n *LarkNotify) ResolveWorkspaceSlug(ctx context.Context, workspaceID string) string {
	wsUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return ""
	}
	ws, err := n.queries.GetWorkspace(ctx, wsUUID)
	if err != nil {
		return ""
	}
	return ws.Slug
}

// ResolveIssueInfoByID looks up an issue and returns IssueInfo populated
// with identifier ("PFX-<n>"), title, and workspace slug. Used by task
// event listeners that only know the issue UUID. Empty on miss.
func (n *LarkNotify) ResolveIssueInfoByID(ctx context.Context, issueID string) (string, IssueInfo, bool) {
	issueUUID, err := util.ParseUUID(issueID)
	if err != nil {
		return "", IssueInfo{}, false
	}
	issue, err := n.queries.GetIssue(ctx, issueUUID)
	if err != nil {
		return "", IssueInfo{}, false
	}
	wsID := util.UUIDToString(issue.WorkspaceID)
	ws, err := n.queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return wsID, IssueInfo{IssueID: issueID, WorkspaceID: wsID, Title: issue.Title}, true
	}
	return wsID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   wsID,
		Identifier:    fmt.Sprintf("%s-%d", ws.IssuePrefix, issue.Number),
		Title:         issue.Title,
		WorkspaceSlug: ws.Slug,
	}, true
}

// ── Card building ────────────────────────────────────────────────────────

// cardButton describes one button rendered into a Lark interactive card.
//
// Exactly one of URL or Value should be set:
//   - URL produces a "navigate" button (clicking opens the link in the
//     user's browser; no callback to multica).
//   - Value produces an action button. Lark POSTs the map back to
//     `POST /api/webhooks/lark` when the user clicks, where the webhook
//     handler dispatches on the `verb` key. Used for P2 in-chat actions
//     (claim, mark_done) that round-trip through multica.
//
// Type controls the Lark visual style ("primary", "default", "danger").
// Empty => "default".
type cardButton struct {
	Text  string
	URL   string
	Value map[string]any
	Type  string
}

// buildCard returns a Lark interactive-card structure. Format follows
// https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/feishu-cards/card-json-structure
//
// We use the "config + header + elements" v1 shape — broadly supported,
// works on mobile and PC clients, and keeps schemas in our test golden
// files compact.
func buildCard(headerTitle, markdownBody string, buttons []cardButton) map[string]any {
	elements := []map[string]any{
		{
			"tag":     "markdown",
			"content": markdownBody,
		},
	}
	if len(buttons) > 0 {
		actions := make([]map[string]any, 0, len(buttons))
		for _, b := range buttons {
			btnType := b.Type
			if btnType == "" {
				btnType = "default"
			}
			btn := map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": b.Text},
				"type": btnType,
			}
			// Action buttons take precedence — when a verb is wired up,
			// stripping the URL avoids Lark opening a new tab on top of
			// the callback. Both fields could theoretically coexist on
			// the Lark side, but the UX of "two things happen at once"
			// is worse than picking one.
			if b.Value != nil {
				btn["value"] = b.Value
			} else if b.URL != "" {
				btn["url"] = b.URL
			}
			actions = append(actions, btn)
		}
		elements = append(elements, map[string]any{
			"tag":     "action",
			"actions": actions,
		})
	}
	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title": map[string]any{"tag": "plain_text", "content": headerTitle},
		},
		"elements": elements,
	}
}

// CardJSON is a small helper exposed for tests / golden-file checks.
func CardJSON(card any) string {
	b, _ := json.Marshal(card)
	return string(b)
}

func sliceContains(arr []string, v string) bool {
	for _, s := range arr {
		if s == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
