package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// larkNotifier is the subset of *service.LarkNotify used by the event
// listeners. Extracted as an interface so tests can verify wiring without
// constructing the full notifier (which needs DB, Lark config, workers).
type larkNotifier interface {
	NotifyIssueCreated(ctx context.Context, workspaceID string, info service.IssueInfo, hasAssignee bool, assigneeUserID string, hasLarkIssueLink bool, assigneeIsWorkspaceAgent bool, creatorName string)
	NotifyIssueAssigned(ctx context.Context, workspaceID string, info service.IssueInfo, assigneeName string, assigneeUserID string, assigneeIsWorkspaceAgent bool)
	PatchIssueTerminalCards(ctx context.Context, workspaceID string, info service.IssueInfo)
	NotifyTaskCompleted(ctx context.Context, workspaceID string, info service.IssueInfo, hasAssignee bool, assigneeUserID string)
	NotifyTaskFailed(ctx context.Context, workspaceID string, info service.IssueInfo, errSummary string, hasAssignee bool, assigneeUserID string)
	NotifyComment(ctx context.Context, workspaceID string, info service.IssueInfo, authorName, excerpt string)
	ResolveWorkspaceSlug(ctx context.Context, workspaceID string) string
	ResolveIssueInfoByID(ctx context.Context, issueID string) (string, service.IssueInfo, bool)
}

// larkThreadMirror is the subset of *service.LarkThreadService used by
// the thread-bridge event listener.
type larkThreadMirror interface {
	Configured() bool
	MirrorAgentCommentToThread(ctx context.Context, issueID pgtype.UUID, authorName, content, issueURL string)
}

// registerLarkListeners wires the event bus to the Lark notifier.
// Per the integration design (§3, §6.1) the bus is the only producer of
// outbound traffic — listeners translate multica payloads into the
// IssueInfo shape, look up workspace context, and call the service.
//
// We deliberately keep listener wiring conditional: if LarkConfigFromEnv()
// returns an unconfigured set, we still register the listeners but the
// service.dispatch path short-circuits inside Configured() so the bus
// overhead is the only cost on unconfigured deployments. (We could skip
// Subscribe() entirely, but then enabling Lark would require a server
// restart even if a binding is updated via the UI — accepting one extra
// map lookup per event keeps the operator story simpler.)
func registerLarkListeners(bus *events.Bus, notify larkNotifier, queries *db.Queries) {
	if notify == nil {
		return
	}

	// issue:created → routed card (team claim or assignee DM).
	// Skip issues created from a Lark thread — the thread service
	// already posts a confirmation reply into the originating thread.
	bus.Subscribe(protocol.EventIssueCreated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		if src, _ := payload["source"].(string); src == "lark_thread" {
			return
		}
		issue, ok := extractIssueFields(payload["issue"])
		if !ok {
			return
		}
		ctx := context.Background()
		info := larkIssueInfo(ctx, notify, queries, issue)
		hasAssignee := issue.AssigneeID != nil && *issue.AssigneeID != ""
		assigneeUserID := ""
		assigneeIsWorkspaceAgent := false
		if hasAssignee {
			assigneeUserID = *issue.AssigneeID
			assigneeType := ""
			if issue.AssigneeType != nil {
				assigneeType = *issue.AssigneeType
			}
			if assigneeType == "agent" {
				assigneeUserID, assigneeIsWorkspaceAgent = larkResolveAgentOwner(ctx, queries, *issue.AssigneeID)
			}
		}
		hasLarkIssueLink := larkHasIssueLink(ctx, queries, info.IssueID)
		creator := larkActorName(ctx, queries, issue.CreatorType, issue.CreatorID)
		notify.NotifyIssueCreated(ctx, issue.WorkspaceID, info, hasAssignee, assigneeUserID, hasLarkIssueLink, assigneeIsWorkspaceAgent, creator)
	})

	// issue:updated → "assigned" card on assignee change, and in-place
	// patch of active cards when the issue reaches a terminal status.
	// Non-terminal priority/status edits remain silent so the bound chat
	// does not become a noisy field-change feed.
	bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		assigneeChanged, _ := payload["assignee_changed"].(bool)
		statusChanged, _ := payload["status_changed"].(bool)
		issue, ok := extractIssueFields(payload["issue"])
		if !ok {
			return
		}
		ctx := context.Background()
		info := larkIssueInfo(ctx, notify, queries, issue)
		if statusChanged && service.IsTerminalIssueStatus(issue.Status) {
			notify.PatchIssueTerminalCards(ctx, issue.WorkspaceID, info)
			return
		}
		if !assigneeChanged {
			return
		}
		if issue.AssigneeID == nil || *issue.AssigneeID == "" {
			return // unassignment — no card
		}
		assigneeType := ""
		if issue.AssigneeType != nil {
			assigneeType = *issue.AssigneeType
		}
		assigneeUserID := *issue.AssigneeID
		assigneeIsWorkspaceAgent := false
		if assigneeType == "agent" {
			assigneeUserID, assigneeIsWorkspaceAgent = larkResolveAgentOwner(ctx, queries, *issue.AssigneeID)
		}
		name := larkActorName(ctx, queries, assigneeType, *issue.AssigneeID)
		notify.NotifyIssueAssigned(ctx, issue.WorkspaceID, info, name, assigneeUserID, assigneeIsWorkspaceAgent)
	})

	// task:completed and task:failed share the same payload shape (see
	// service/task.go broadcastTaskEvent). We resolve issue context from
	// issue_id when present; chat-only and quick-create tasks fall back to
	// the workspace home link. The assignee is resolved from the issue row
	// so dispatchRouted can DM the right person.
	bus.Subscribe(protocol.EventTaskCompleted, func(e events.Event) {
		wsID, info := larkTaskIssue(notify, e)
		if wsID == "" {
			return
		}
		ctx := context.Background()
		assigneeUserID, hasAssignee := larkTaskAssignee(ctx, queries, e.Payload)
		notify.NotifyTaskCompleted(ctx, wsID, info, hasAssignee, assigneeUserID)
	})
	bus.Subscribe(protocol.EventTaskFailed, func(e events.Event) {
		wsID, info := larkTaskIssue(notify, e)
		if wsID == "" {
			return
		}
		ctx := context.Background()
		assigneeUserID, hasAssignee := larkTaskAssignee(ctx, queries, e.Payload)
		// task:failed payloads are inconsistent across producers:
		//   - service.broadcastTaskEvent (the normal path) emits only
		//     task_id / agent_id / issue_id / status — no error text.
		//   - runtime_sweeper emits failure_reason.
		// We try the payload first (cheap), then fall back to a
		// GetAgentTask lookup so the card always carries some hint.
		errSummary := larkTaskFailureSummary(ctx, queries, e.Payload)
		notify.NotifyTaskFailed(ctx, wsID, info, errSummary, hasAssignee, assigneeUserID)
	})

	// comment:created → only when the comment @mentions a person or agent
	// (not just an [issue](mention://issue/...) link), so the bound chat
	// doesn't turn into a noisy comment feed.
	bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		issueID, authorType, authorID, content, ok := extractCommentFields(payload["comment"])
		if !ok {
			return
		}
		if !hasUserMention(content) {
			return
		}
		ctx := context.Background()
		_, info, ok := notify.ResolveIssueInfoByID(ctx, issueID)
		if !ok {
			return
		}
		author := larkActorName(ctx, queries, authorType, authorID)
		notify.NotifyComment(ctx, e.WorkspaceID, info, author, content)
	})
}

// registerLarkThreadListeners wires the event bus to the Lark thread
// bridge (P5 outbound). Agent-authored comments on issues that originated
// from a Lark thread (lark_issue_link row exists) get mirrored back into
// the same thread as a "[multica]" reply, so humans in Lark can keep
// answering the agent without leaving the chat.
//
// Loop-prevention: the inbound side (handler.handleLarkMessageEvent's
// reply branch) stamps source="lark_thread" on its publish; we skip those
// here so an inbound reply doesn't echo back into the same thread it
// arrived from. Agent comments published by TaskService never set source,
// so they pass the filter.
//
// Filter: agent author only. Per the design's "agent 写 Q" semantics
// (§6.6) — human comments in the multica UI are NOT mirrored, so a single
// reviewer reading the issue page doesn't double-spam the Lark thread.
func registerLarkThreadListeners(bus *events.Bus, larkThread larkThreadMirror, queries *db.Queries) {
	if larkThread == nil {
		return
	}
	bus.Subscribe(protocol.EventCommentCreated, func(e events.Event) {
		if !larkThread.Configured() {
			return
		}
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		if src, _ := payload["source"].(string); src == "lark_thread" {
			return
		}
		issueID, authorType, authorID, content, ok := extractCommentFields(payload["comment"])
		if !ok || authorType != "agent" {
			return
		}
		ctx := context.Background()
		issueUUID, err := util.ParseUUID(issueID)
		if err != nil {
			return
		}
		name := larkActorName(ctx, queries, authorType, authorID)
		issueURL := larkBuildIssueURL(ctx, queries, issueID)
		larkThread.MirrorAgentCommentToThread(ctx, issueUUID, name, content, issueURL)
	})
}

// larkBuildIssueURL constructs a deep-link to the issue for card buttons.
// Returns "" on any resolution failure — callers omit the button.
func larkBuildIssueURL(ctx context.Context, queries *db.Queries, issueID string) string {
	if queries == nil {
		return ""
	}
	issueUUID, err := util.ParseUUID(issueID)
	if err != nil {
		return ""
	}
	issue, err := queries.GetIssue(ctx, issueUUID)
	if err != nil {
		return ""
	}
	ws, err := queries.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return ""
	}
	frontend := strings.TrimRight(strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN")), "/")
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	return fmt.Sprintf("%s/%s/issues/%s-%d", frontend, ws.Slug, ws.IssuePrefix, issue.Number)
}

// larkIssueInfo projects an IssueResponse into the slug-aware IssueInfo
// the notifier consumes. Falls back to whatever fields are non-empty so a
// partially-populated payload still yields a usable card.
func larkIssueInfo(ctx context.Context, notify larkNotifier, queries *db.Queries, issue handler.IssueResponse) service.IssueInfo {
	slug := ""
	if issue.WorkspaceID != "" {
		slug = notify.ResolveWorkspaceSlug(ctx, issue.WorkspaceID)
	}
	identifier := issue.Identifier
	if identifier == "" && issue.WorkspaceID != "" && issue.Number != 0 {
		// IssueResponse from autopilot/internal paths sometimes omits the
		// Identifier — synthesize from prefix + number so the card title
		// still reads as a real ticket reference.
		if wsUUID, err := util.ParseUUID(issue.WorkspaceID); err == nil {
			if ws, err := queries.GetWorkspace(ctx, wsUUID); err == nil {
				identifier = fmt.Sprintf("%s-%d", ws.IssuePrefix, issue.Number)
			}
		}
	}
	dueDate := ""
	if issue.DueDate != nil {
		dueDate = *issue.DueDate
	}
	return service.IssueInfo{
		IssueID:       issue.ID,
		WorkspaceID:   issue.WorkspaceID,
		Identifier:    identifier,
		Title:         issue.Title,
		WorkspaceSlug: slug,
		Status:        issue.Status,
		Priority:      issue.Priority,
		DueDate:       dueDate,
	}
}

// larkTaskIssue resolves the workspace + issue for a task event using the
// payload's issue_id (which broadcastTaskEvent always sets).
func larkTaskIssue(notify larkNotifier, e events.Event) (string, service.IssueInfo) {
	issueID, _ := taskPayloadField(e.Payload, "issue_id")
	if issueID == "" {
		return e.WorkspaceID, service.IssueInfo{WorkspaceSlug: notify.ResolveWorkspaceSlug(context.Background(), e.WorkspaceID)}
	}
	wsID, info, ok := notify.ResolveIssueInfoByID(context.Background(), issueID)
	if !ok || wsID == "" {
		return e.WorkspaceID, service.IssueInfo{WorkspaceSlug: notify.ResolveWorkspaceSlug(context.Background(), e.WorkspaceID)}
	}
	return wsID, info
}

// larkTaskAssignee resolves the issue's assignee from the task event
// payload's issue_id. For local agent assignees it resolves the owner's
// user ID so the DM reaches a human. Returns ("", false) when the issue
// has no personal recipient (including workspace/cloud agents) or the
// lookup fails — callers pass these through to dispatchRouted which
// handles the no-assignee routing (e.g. team-chat public blocker for
// task:failed).
func larkTaskAssignee(ctx context.Context, queries *db.Queries, payload any) (userID string, hasAssignee bool) {
	if queries == nil {
		return "", false
	}
	issueID, ok := taskPayloadField(payload, "issue_id")
	if !ok || issueID == "" {
		return "", false
	}
	id, err := util.ParseUUID(issueID)
	if err != nil {
		return "", false
	}
	issue, err := queries.GetIssue(ctx, id)
	if err != nil {
		return "", false
	}
	if !issue.AssigneeID.Valid {
		return "", false
	}
	assigneeID := util.UUIDToString(issue.AssigneeID)
	if assigneeID == "" {
		return "", false
	}
	assigneeType := ""
	if issue.AssigneeType.Valid {
		assigneeType = issue.AssigneeType.String
	}
	if assigneeType == "agent" {
		ownerID, isWorkspaceAgent := larkResolveAgentOwner(ctx, queries, assigneeID)
		if isWorkspaceAgent || ownerID == "" {
			return "", false
		}
		return ownerID, true
	}
	return assigneeID, true
}

func taskPayloadField(payload any, key string) (string, bool) {
	m, ok := payload.(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := m[key].(string)
	return s, ok
}

// larkTaskFailureSummary pulls a one-line error for the failed-task card.
//
// The two `failure_reason` semantics are NOT the same and the priority
// order below reflects that:
//   - The PAYLOAD `failure_reason` is set by runtime_sweeper to a
//     specific phrase ("runtime offline", etc.) — high-signal, prefer.
//   - The DB row's `FailureReason` column is set by FailTask to a
//     coarse classifier like "agent_error" used by the auto-retry
//     logic — low-signal, only useful as a last resort.
//   - The DB row's `Error` column is set to the actual error message —
//     this is the actionable text the user wants to see on the card.
//
// So: payload.failure_reason → payload.error → row.Error → row.FailureReason.
// Returns "" when no hint exists; the notifier renders the card without
// the code block in that case.
func larkTaskFailureSummary(ctx context.Context, queries *db.Queries, payload any) string {
	if s, ok := taskPayloadField(payload, "failure_reason"); ok && s != "" {
		return s
	}
	if s, ok := taskPayloadField(payload, "error"); ok && s != "" {
		return s
	}
	taskID, ok := taskPayloadField(payload, "task_id")
	if !ok {
		return ""
	}
	id, err := util.ParseUUID(taskID)
	if err != nil {
		return ""
	}
	task, err := queries.GetAgentTask(ctx, id)
	if err != nil {
		return ""
	}
	if task.Error.Valid && task.Error.String != "" {
		return task.Error.String
	}
	if task.FailureReason.Valid && task.FailureReason.String != "" {
		return task.FailureReason.String
	}
	return ""
}

// extractCommentFields normalises a comment payload that may be either
// handler.CommentResponse (user-facing handler path) or map[string]any
// (TaskService.createAgentComment publishes the map form). The original
// listener only matched the struct form, which silently dropped every
// agent-authored comment from the Lark feed.
func extractCommentFields(v any) (issueID, authorType, authorID, content string, ok bool) {
	if c, isResp := v.(handler.CommentResponse); isResp {
		return c.IssueID, c.AuthorType, c.AuthorID, c.Content, c.IssueID != "" && c.AuthorID != ""
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return "", "", "", "", false
	}
	issueID, _ = m["issue_id"].(string)
	authorType, _ = m["author_type"].(string)
	authorID, _ = m["author_id"].(string)
	content, _ = m["content"].(string)
	ok = issueID != "" && authorID != ""
	return
}

// hasUserMention returns true when the content references at least one
// member, agent, squad, or @all — i.e. a notification target. We
// deliberately ignore mention://issue/<id> references because issue
// links are how multica renders MUL-XYZ cross-links in markdown and
// they don't represent an "@somebody" signal.
func hasUserMention(content string) bool {
	for _, m := range util.ParseMentions(content) {
		switch m.Type {
		case "member", "agent", "squad", "all":
			return true
		}
	}
	return false
}

// larkActorName looks up a display name for an actor (member or agent).
// Returns "" when the lookup fails; callers treat that as "render without
// the byline" rather than failing the card.
func larkActorName(ctx context.Context, queries *db.Queries, actorType, actorID string) string {
	if actorID == "" || queries == nil {
		return ""
	}
	id, err := util.ParseUUID(actorID)
	if err != nil {
		return ""
	}
	switch actorType {
	case "member":
		user, err := queries.GetUser(ctx, id)
		if err != nil {
			return ""
		}
		if user.Name != "" {
			return user.Name
		}
		return user.Email
	case "agent":
		agent, err := queries.GetAgent(ctx, id)
		if err != nil {
			return ""
		}
		return agent.Name
	}
	return ""
}

// larkResolveAgentOwner looks up an agent and returns the DM target
// user ID and whether the agent is a workspace-level (cloud) agent.
//
// Local (daemon) agents have an owner — the person running the daemon.
// The returned userID is the owner's user UUID so the DM goes to them.
// Workspace (cloud) agents have no personal owner; the returned userID
// is empty and isWorkspaceAgent is true, which routes to team chat.
func larkResolveAgentOwner(ctx context.Context, queries *db.Queries, agentID string) (userID string, isWorkspaceAgent bool) {
	id, err := util.ParseUUID(agentID)
	if err != nil {
		return "", false
	}
	agent, err := queries.GetAgent(ctx, id)
	if err != nil {
		return "", false
	}
	if agent.RuntimeMode == "cloud" {
		return "", true
	}
	if agent.OwnerID.Valid {
		return util.UUIDToString(agent.OwnerID), false
	}
	return "", false
}

// larkHasIssueLink checks whether an issue has a lark_issue_link row
// (i.e. it was created from a Lark thread). Used to populate
// LarkRoutingConditions.HasLarkIssueLink.
func larkHasIssueLink(ctx context.Context, queries *db.Queries, issueID string) bool {
	if issueID == "" {
		return false
	}
	id, err := util.ParseUUID(issueID)
	if err != nil {
		return false
	}
	_, err = queries.GetLarkIssueLinkByIssueID(ctx, id)
	return err == nil
}
