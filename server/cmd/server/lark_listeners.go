package main

import (
	"context"
	"fmt"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

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
func registerLarkListeners(bus *events.Bus, notify *service.LarkNotify, queries *db.Queries) {
	if notify == nil {
		return
	}

	// issue:created → "new issue" card with Claim/View button.
	bus.Subscribe(protocol.EventIssueCreated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		issue, ok := extractIssueFields(payload["issue"])
		if !ok {
			return
		}
		ctx := context.Background()
		info := larkIssueInfo(ctx, notify, queries, issue)
		hasAssignee := issue.AssigneeID != nil && *issue.AssigneeID != ""
		creator := larkActorName(ctx, queries, issue.CreatorType, issue.CreatorID)
		notify.NotifyIssueCreated(ctx, issue.WorkspaceID, info, hasAssignee, creator)
	})

	// issue:updated → "assigned" card on assignee change.
	// Other fields (priority, status, etc.) are intentionally not surfaced
	// in P1 to keep the bound chat from becoming noisy. P5+ can layer in
	// per-event toggles when the team validates which signals matter.
	bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		assigneeChanged, _ := payload["assignee_changed"].(bool)
		if !assigneeChanged {
			return
		}
		issue, ok := extractIssueFields(payload["issue"])
		if !ok {
			return
		}
		if issue.AssigneeID == nil || *issue.AssigneeID == "" {
			return // unassignment — no card
		}
		ctx := context.Background()
		info := larkIssueInfo(ctx, notify, queries, issue)
		assigneeType := ""
		if issue.AssigneeType != nil {
			assigneeType = *issue.AssigneeType
		}
		name := larkActorName(ctx, queries, assigneeType, *issue.AssigneeID)
		notify.NotifyIssueAssigned(ctx, issue.WorkspaceID, info, name)
	})

	// task:completed and task:failed share the same payload shape (see
	// service/task.go broadcastTaskEvent). We resolve issue context from
	// issue_id when present; chat-only and quick-create tasks fall back to
	// the workspace home link.
	bus.Subscribe(protocol.EventTaskCompleted, func(e events.Event) {
		wsID, info := larkTaskIssue(notify, e)
		if wsID == "" {
			return
		}
		notify.NotifyTaskCompleted(context.Background(), wsID, info)
	})
	bus.Subscribe(protocol.EventTaskFailed, func(e events.Event) {
		wsID, info := larkTaskIssue(notify, e)
		if wsID == "" {
			return
		}
		ctx := context.Background()
		// task:failed payloads are inconsistent across producers:
		//   - service.broadcastTaskEvent (the normal path) emits only
		//     task_id / agent_id / issue_id / status — no error text.
		//   - runtime_sweeper emits failure_reason.
		// We try the payload first (cheap), then fall back to a
		// GetAgentTask lookup so the card always carries some hint.
		errSummary := larkTaskFailureSummary(ctx, queries, e.Payload)
		notify.NotifyTaskFailed(ctx, wsID, info, errSummary)
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

// larkIssueInfo projects an IssueResponse into the slug-aware IssueInfo
// the notifier consumes. Falls back to whatever fields are non-empty so a
// partially-populated payload still yields a usable card.
func larkIssueInfo(ctx context.Context, notify *service.LarkNotify, queries *db.Queries, issue handler.IssueResponse) service.IssueInfo {
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
	return service.IssueInfo{
		Identifier:    identifier,
		Title:         issue.Title,
		WorkspaceSlug: slug,
	}
}

// larkTaskIssue resolves the workspace + issue for a task event using the
// payload's issue_id (which broadcastTaskEvent always sets).
func larkTaskIssue(notify *service.LarkNotify, e events.Event) (string, service.IssueInfo) {
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
	if actorID == "" {
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
