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
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/internal/util"
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

// larkJob is what the bus goroutine enqueues for workers to dispatch.
// We capture the rendered card + chat_id, not a closure over the binding
// row, so the worker doesn't pin queries / context past dispatch().
type larkJob struct {
	chatID string
	card   any
	event  string
	wsID   string
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
			if err := n.client.SendInteractiveCard(ctx, job.chatID, job.card); err != nil {
				n.log.Warn("lark: send failed", "event", job.event, "ws", job.wsID, "err", err)
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
type IssueInfo struct {
	Identifier string
	Title      string
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

// NotifyIssueCreated emits an "issue created" card with a Claim button.
// hasAssignee gates the button label: assigned issues skip the claim CTA
// and just offer a View action.
func (n *LarkNotify) NotifyIssueCreated(ctx context.Context, workspaceID string, info IssueInfo, hasAssignee bool, creatorName string) {
	n.dispatch(ctx, workspaceID, protocol.EventIssueCreated, func() any {
		title := fmt.Sprintf("📝 New issue: %s", info.Identifier)
		desc := info.Title
		if creatorName != "" {
			desc = fmt.Sprintf("%s\n\n_Created by %s_", desc, creatorName)
		}
		actionLabel := "Claim"
		if hasAssignee {
			actionLabel = "View"
		}
		return buildCard(title, desc, []cardButton{{Text: actionLabel, URL: n.IssueURL(info)}})
	})
}

// NotifyIssueAssigned emits an "issue assigned" card on assignee change.
// assigneeName is best-effort — empty string just omits the line.
func (n *LarkNotify) NotifyIssueAssigned(ctx context.Context, workspaceID string, info IssueInfo, assigneeName string) {
	n.dispatch(ctx, workspaceID, protocol.EventIssueUpdated, func() any {
		title := fmt.Sprintf("👤 Assigned: %s", info.Identifier)
		desc := info.Title
		if assigneeName != "" {
			desc = fmt.Sprintf("%s\n\n_Assignee: %s_", desc, assigneeName)
		}
		return buildCard(title, desc, []cardButton{{Text: "View", URL: n.IssueURL(info)}})
	})
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
	// Stop() flips this BEFORE close(stopCh) so a bus goroutine that
	// observed started=true a moment ago doesn't try to send on a queue
	// whose readers have already exited. The check is best-effort (no
	// memory barrier between dispatch and Stop), but combined with the
	// non-blocking select below the worst case is a single dropped card.
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
	job := larkJob{chatID: binding.ChatID, card: card, event: eventKind, wsID: workspaceID}

	// Pre-Start (e.g. unit test) or unconfigured deployments: workers
	// never ran, so the channel will block forever. In that case do the
	// send inline so tests can assert on it deterministically. Otherwise
	// enqueue non-blocking — Lark outages get dropped, not back-pressured.
	if !n.started.Load() {
		send_ctx, cancel := context.WithTimeout(ctx, n.sendTimeout)
		defer cancel()
		if err := n.client.SendInteractiveCard(send_ctx, job.chatID, job.card); err != nil {
			n.log.Warn("lark: send failed", "event", job.event, "ws", job.wsID, "err", err)
		}
		return
	}
	select {
	case n.jobs <- job:
	default:
		n.log.Warn("lark: queue full, dropping event", "event", eventKind, "ws", workspaceID)
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
		return wsID, IssueInfo{Title: issue.Title}, true
	}
	return wsID, IssueInfo{
		Identifier:    fmt.Sprintf("%s-%d", ws.IssuePrefix, issue.Number),
		Title:         issue.Title,
		WorkspaceSlug: ws.Slug,
	}, true
}

// ── Card building ────────────────────────────────────────────────────────

type cardButton struct {
	Text string
	URL  string
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
			actions = append(actions, map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": b.Text},
				"url":  b.URL,
				"type": "default",
			})
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
