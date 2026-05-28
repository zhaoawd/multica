package main

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// --- mock types ---

type notifyCall struct {
	method string
	args   map[string]any
}

type mockLarkNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
}

func (m *mockLarkNotifier) record(method string, args map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, notifyCall{method: method, args: args})
}

func (m *mockLarkNotifier) getCalls() []notifyCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]notifyCall, len(m.calls))
	copy(cp, m.calls)
	return cp
}

func (m *mockLarkNotifier) NotifyIssueCreated(_ context.Context, workspaceID string, info service.IssueInfo, hasAssignee bool, assigneeUserID string, hasLarkIssueLink bool, assigneeIsWorkspaceAgent bool, creatorName string) {
	m.record("NotifyIssueCreated", map[string]any{
		"workspaceID":              workspaceID,
		"info":                     info,
		"hasAssignee":              hasAssignee,
		"assigneeUserID":           assigneeUserID,
		"hasLarkIssueLink":         hasLarkIssueLink,
		"assigneeIsWorkspaceAgent": assigneeIsWorkspaceAgent,
		"creatorName":              creatorName,
	})
}

func (m *mockLarkNotifier) NotifyIssueAssigned(_ context.Context, workspaceID string, info service.IssueInfo, assigneeName string, assigneeUserID string, assigneeIsWorkspaceAgent bool) {
	m.record("NotifyIssueAssigned", map[string]any{
		"workspaceID":              workspaceID,
		"info":                     info,
		"assigneeName":             assigneeName,
		"assigneeUserID":           assigneeUserID,
		"assigneeIsWorkspaceAgent": assigneeIsWorkspaceAgent,
	})
}

func (m *mockLarkNotifier) PatchIssueTerminalCards(_ context.Context, workspaceID string, info service.IssueInfo) {
	m.record("PatchIssueTerminalCards", map[string]any{
		"workspaceID": workspaceID,
		"info":        info,
	})
}

func (m *mockLarkNotifier) NotifyTaskCompleted(_ context.Context, workspaceID string, info service.IssueInfo, hasAssignee bool, assigneeUserID string) {
	m.record("NotifyTaskCompleted", map[string]any{
		"workspaceID":    workspaceID,
		"info":           info,
		"hasAssignee":    hasAssignee,
		"assigneeUserID": assigneeUserID,
	})
}

func (m *mockLarkNotifier) NotifyTaskFailed(_ context.Context, workspaceID string, info service.IssueInfo, errSummary string, hasAssignee bool, assigneeUserID string) {
	m.record("NotifyTaskFailed", map[string]any{
		"workspaceID":    workspaceID,
		"info":           info,
		"errSummary":     errSummary,
		"hasAssignee":    hasAssignee,
		"assigneeUserID": assigneeUserID,
	})
}

func (m *mockLarkNotifier) NotifyComment(_ context.Context, workspaceID string, info service.IssueInfo, authorName, excerpt string) {
	m.record("NotifyComment", map[string]any{
		"workspaceID": workspaceID,
		"info":        info,
		"authorName":  authorName,
		"excerpt":     excerpt,
	})
}

func (m *mockLarkNotifier) ResolveWorkspaceSlug(_ context.Context, _ string) string {
	return "test-ws"
}

func (m *mockLarkNotifier) ResolveIssueInfoByID(_ context.Context, issueID string) (string, service.IssueInfo, bool) {
	return "ws-uuid", service.IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   "ws-uuid",
		Identifier:    "TEST-1",
		Title:         "resolved issue",
		WorkspaceSlug: "test-ws",
	}, true
}

type mirrorCall struct {
	issueID    pgtype.UUID
	authorName string
	content    string
	issueURL   string
}

type mockLarkThreadMirror struct {
	mu         sync.Mutex
	configured bool
	calls      []mirrorCall
}

func (m *mockLarkThreadMirror) Configured() bool { return m.configured }

func (m *mockLarkThreadMirror) MirrorAgentCommentToThread(_ context.Context, issueID pgtype.UUID, authorName, content, issueURL string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, mirrorCall{issueID: issueID, authorName: authorName, content: content, issueURL: issueURL})
}

func (m *mockLarkThreadMirror) getCalls() []mirrorCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]mirrorCall, len(m.calls))
	copy(cp, m.calls)
	return cp
}

// --- helpers ---

func issuePayload(issue handler.IssueResponse) map[string]any {
	return map[string]any{"issue": issue}
}

func issuePayloadWithSource(issue handler.IssueResponse, source string) map[string]any {
	return map[string]any{"issue": issue, "source": source}
}

func issueUpdatedPayload(issue handler.IssueResponse, assigneeChanged, statusChanged bool) map[string]any {
	return map[string]any{
		"issue":            issue,
		"assignee_changed": assigneeChanged,
		"status_changed":   statusChanged,
	}
}

func commentPayload(comment handler.CommentResponse) map[string]any {
	return map[string]any{"comment": comment}
}

func commentPayloadWithSource(comment handler.CommentResponse, source string) map[string]any {
	return map[string]any{"comment": comment, "source": source}
}

func taskPayload(issueID string) map[string]any {
	return map[string]any{"issue_id": issueID, "task_id": "task-uuid"}
}

func taskPayloadWithError(issueID, failureReason string) map[string]any {
	return map[string]any{"issue_id": issueID, "task_id": "task-uuid", "failure_reason": failureReason}
}

func createLarkTaskAssigneeIssue(t *testing.T, assigneeType, assigneeID string, number int) string {
	t.Helper()
	testPool.Exec(context.Background(), `DELETE FROM issue WHERE workspace_id = $1 AND number = $2`, testWorkspaceID, number)

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (
			workspace_id, title, status, priority,
			assignee_type, assignee_id, creator_type, creator_id, number
		)
		VALUES ($1, $2, 'todo', 'medium', $3, $4, 'member', $5, $6)
		RETURNING id
	`, testWorkspaceID, "Lark task assignee test", assigneeType, assigneeID, testUserID, number).Scan(&issueID); err != nil {
		t.Fatalf("create lark task assignee issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})
	return issueID
}

func createLarkTaskAssigneeAgent(t *testing.T, runtimeMode, ownerID string) string {
	t.Helper()
	var runtimeID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status,
			device_info, metadata, last_seen_at, owner_id
		)
		VALUES ($1, NULL, $2, $3, $4, 'online', $5, '{}'::jsonb, now(), $6)
		RETURNING id
	`, testWorkspaceID, "Lark Task Assignee Runtime", runtimeMode, "lark_task_assignee_test", "test device", ownerID).Scan(&runtimeID); err != nil {
		t.Fatalf("create lark task assignee runtime: %v", err)
	}

	var agentID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id
		)
		VALUES ($1, $2, '', $3, '{}'::jsonb, $4, 'workspace', 1, $5)
		RETURNING id
	`, testWorkspaceID, "Lark Task Assignee Agent", runtimeMode, runtimeID, ownerID).Scan(&agentID); err != nil {
		t.Fatalf("create lark task assignee agent: %v", err)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM agent WHERE id = $1`, agentID)
		testPool.Exec(ctx, `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})
	return agentID
}

// baseIssue returns a minimal IssueResponse with no assignee.
func baseIssue() handler.IssueResponse {
	return handler.IssueResponse{
		ID:          "issue-uuid-1",
		WorkspaceID: "ws-uuid",
		Number:      1,
		Identifier:  "TEST-1",
		Title:       "Fix login bug",
		Status:      "todo",
		Priority:    "medium",
		CreatorType: "member",
		CreatorID:   "creator-uuid",
	}
}

// --- registerLarkListeners tests ---

func TestLarkListeners_NilNotifier_DoesNotPanic(t *testing.T) {
	bus := events.New()
	registerLarkListeners(bus, nil, nil)
	bus.Publish(events.Event{Type: protocol.EventIssueCreated, Payload: issuePayload(baseIssue())})
}

func TestLarkListeners_IssueCreated_NoAssignee(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	bus.Publish(events.Event{
		Type:    protocol.EventIssueCreated,
		Payload: issuePayload(baseIssue()),
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(calls), calls)
	}
	if calls[0].method != "NotifyIssueCreated" {
		t.Errorf("expected NotifyIssueCreated, got %s", calls[0].method)
	}
	if calls[0].args["hasAssignee"].(bool) {
		t.Error("hasAssignee should be false for unassigned issue")
	}
	if calls[0].args["assigneeUserID"].(string) != "" {
		t.Error("assigneeUserID should be empty")
	}
}

func TestLarkListeners_IssueCreated_WithMemberAssignee(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	issue := baseIssue()
	issue.AssigneeType = strPtr("member")
	issue.AssigneeID = strPtr("assignee-uuid")

	bus.Publish(events.Event{
		Type:    protocol.EventIssueCreated,
		Payload: issuePayload(issue),
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if !calls[0].args["hasAssignee"].(bool) {
		t.Error("hasAssignee should be true")
	}
	if calls[0].args["assigneeUserID"].(string) != "assignee-uuid" {
		t.Errorf("expected assignee-uuid, got %s", calls[0].args["assigneeUserID"])
	}
	if calls[0].args["assigneeIsWorkspaceAgent"].(bool) {
		t.Error("assigneeIsWorkspaceAgent should be false for member")
	}
}

func TestLarkListeners_IssueCreated_LarkThreadSource_Skipped(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	bus.Publish(events.Event{
		Type:    protocol.EventIssueCreated,
		Payload: issuePayloadWithSource(baseIssue(), "lark_thread"),
	})

	if len(mock.getCalls()) != 0 {
		t.Error("issue created from lark_thread should be skipped to prevent loop")
	}
}

func TestLarkListeners_IssueCreated_NonLarkSource_NotSkipped(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	bus.Publish(events.Event{
		Type:    protocol.EventIssueCreated,
		Payload: issuePayloadWithSource(baseIssue(), "web"),
	})

	if len(mock.getCalls()) != 1 {
		t.Error("non-lark_thread source should not be skipped")
	}
}

func TestLarkListeners_IssueCreated_InvalidPayload_Silent(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	bus.Publish(events.Event{Type: protocol.EventIssueCreated, Payload: "not a map"})
	bus.Publish(events.Event{Type: protocol.EventIssueCreated, Payload: map[string]any{"issue": "not an issue"}})
	bus.Publish(events.Event{Type: protocol.EventIssueCreated, Payload: map[string]any{}})

	if len(mock.getCalls()) != 0 {
		t.Error("invalid payloads should be silently dropped")
	}
}

func TestLarkListeners_IssueUpdated_AssigneeChanged(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	issue := baseIssue()
	issue.AssigneeType = strPtr("member")
	issue.AssigneeID = strPtr("new-assignee-uuid")

	bus.Publish(events.Event{
		Type:    protocol.EventIssueUpdated,
		Payload: issueUpdatedPayload(issue, true, false),
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d: %+v", len(calls), calls)
	}
	if calls[0].method != "NotifyIssueAssigned" {
		t.Errorf("expected NotifyIssueAssigned, got %s", calls[0].method)
	}
	if calls[0].args["assigneeUserID"].(string) != "new-assignee-uuid" {
		t.Errorf("expected new-assignee-uuid, got %s", calls[0].args["assigneeUserID"])
	}
}

func TestLarkListeners_IssueUpdated_Unassignment_Silent(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	issue := baseIssue()
	// AssigneeID nil = unassignment

	bus.Publish(events.Event{
		Type:    protocol.EventIssueUpdated,
		Payload: issueUpdatedPayload(issue, true, false),
	})

	if len(mock.getCalls()) != 0 {
		t.Error("unassignment should not produce a card")
	}
}

func TestLarkListeners_IssueUpdated_EmptyAssigneeID_Silent(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	issue := baseIssue()
	issue.AssigneeID = strPtr("")

	bus.Publish(events.Event{
		Type:    protocol.EventIssueUpdated,
		Payload: issueUpdatedPayload(issue, true, false),
	})

	if len(mock.getCalls()) != 0 {
		t.Error("empty assignee ID should not produce a card")
	}
}

func TestLarkListeners_IssueUpdated_NoChange_Silent(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	issue := baseIssue()
	issue.AssigneeType = strPtr("member")
	issue.AssigneeID = strPtr("assignee-uuid")

	bus.Publish(events.Event{
		Type:    protocol.EventIssueUpdated,
		Payload: issueUpdatedPayload(issue, false, false),
	})

	if len(mock.getCalls()) != 0 {
		t.Error("no assignee change and no status change should be silent")
	}
}

func TestLarkListeners_IssueUpdated_TerminalStatus_PatchCards(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	for _, status := range []string{"done", "cancelled"} {
		mock.calls = nil
		issue := baseIssue()
		issue.Status = status

		bus.Publish(events.Event{
			Type:    protocol.EventIssueUpdated,
			Payload: issueUpdatedPayload(issue, false, true),
		})

		calls := mock.getCalls()
		if len(calls) != 1 {
			t.Fatalf("status=%s: expected 1 call, got %d", status, len(calls))
		}
		if calls[0].method != "PatchIssueTerminalCards" {
			t.Errorf("status=%s: expected PatchIssueTerminalCards, got %s", status, calls[0].method)
		}
	}
}

func TestLarkListeners_IssueUpdated_TerminalStatus_TakesPriorityOverAssignee(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	issue := baseIssue()
	issue.Status = "done"
	issue.AssigneeType = strPtr("member")
	issue.AssigneeID = strPtr("someone")

	bus.Publish(events.Event{
		Type:    protocol.EventIssueUpdated,
		Payload: issueUpdatedPayload(issue, true, true),
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].method != "PatchIssueTerminalCards" {
		t.Errorf("terminal status should take priority; got %s", calls[0].method)
	}
}

func TestLarkListeners_IssueUpdated_NonTerminalStatusChange_Silent(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	issue := baseIssue()
	issue.Status = "in_progress"

	bus.Publish(events.Event{
		Type:    protocol.EventIssueUpdated,
		Payload: issueUpdatedPayload(issue, false, true),
	})

	if len(mock.getCalls()) != 0 {
		t.Error("non-terminal status change should be silent")
	}
}

func TestLarkListeners_TaskCompleted(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	bus.Publish(events.Event{
		Type:        protocol.EventTaskCompleted,
		WorkspaceID: "ws-uuid",
		Payload:     taskPayload("issue-uuid-1"),
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].method != "NotifyTaskCompleted" {
		t.Errorf("expected NotifyTaskCompleted, got %s", calls[0].method)
	}
}

func TestLarkListeners_TaskFailed_WithPayloadReason(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	bus.Publish(events.Event{
		Type:        protocol.EventTaskFailed,
		WorkspaceID: "ws-uuid",
		Payload:     taskPayloadWithError("issue-uuid-1", "runtime offline"),
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].method != "NotifyTaskFailed" {
		t.Errorf("expected NotifyTaskFailed, got %s", calls[0].method)
	}
	if calls[0].args["errSummary"].(string) != "runtime offline" {
		t.Errorf("expected 'runtime offline', got %q", calls[0].args["errSummary"])
	}
}

func TestLarkListeners_TaskFailed_NoIssueID_StillFires(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	bus.Publish(events.Event{
		Type:        protocol.EventTaskFailed,
		WorkspaceID: "ws-uuid",
		Payload:     map[string]any{"task_id": "t1"},
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("task without issue_id should still fire (fallback to workspace home): got %d calls", len(calls))
	}
}

func TestLarkTaskAssignee_MemberAssignee(t *testing.T) {
	queries := db.New(testPool)
	issueID := createLarkTaskAssigneeIssue(t, "member", testUserID, 9101)

	userID, hasAssignee := larkTaskAssignee(context.Background(), queries, taskPayload(issueID))
	if !hasAssignee {
		t.Fatal("expected member-assigned issue to report hasAssignee=true")
	}
	if userID != testUserID {
		t.Fatalf("expected assignee user %s, got %s", testUserID, userID)
	}
}

func TestLarkTaskAssignee_LocalAgentResolvesOwner(t *testing.T) {
	queries := db.New(testPool)
	agentID := createLarkTaskAssigneeAgent(t, "local", testUserID)
	issueID := createLarkTaskAssigneeIssue(t, "agent", agentID, 9102)

	userID, hasAssignee := larkTaskAssignee(context.Background(), queries, taskPayload(issueID))
	if !hasAssignee {
		t.Fatal("expected local-agent-assigned issue to report hasAssignee=true")
	}
	if userID != testUserID {
		t.Fatalf("expected local agent owner %s, got %s", testUserID, userID)
	}
}

func TestLarkTaskAssignee_CloudAgentHasNoPersonalRecipient(t *testing.T) {
	queries := db.New(testPool)
	agentID := createLarkTaskAssigneeAgent(t, "cloud", testUserID)
	issueID := createLarkTaskAssigneeIssue(t, "agent", agentID, 9103)

	userID, hasAssignee := larkTaskAssignee(context.Background(), queries, taskPayload(issueID))
	if hasAssignee {
		t.Fatalf("cloud-agent task should not report a personal assignee; got userID=%q", userID)
	}
	if userID != "" {
		t.Fatalf("expected empty personal recipient for cloud agent, got %s", userID)
	}
}

func TestLarkListeners_CommentCreated_WithMention_NotifiesComment(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	comment := handler.CommentResponse{
		ID:         "comment-1",
		IssueID:    "issue-uuid-1",
		AuthorType: "member",
		AuthorID:   "author-uuid",
		Content:    "Hey [@Alice](mention://member/aaaa-aaaa-aaaa) can you check this?",
	}

	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: "ws-uuid",
		Payload:     commentPayload(comment),
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].method != "NotifyComment" {
		t.Errorf("expected NotifyComment, got %s", calls[0].method)
	}
	if calls[0].args["excerpt"].(string) != comment.Content {
		t.Error("content should be passed through")
	}
}

func TestLarkListeners_CommentCreated_NoMention_Silent(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	comment := handler.CommentResponse{
		ID:         "comment-1",
		IssueID:    "issue-uuid-1",
		AuthorType: "member",
		AuthorID:   "author-uuid",
		Content:    "Looks good, merging now.",
	}

	bus.Publish(events.Event{
		Type:    protocol.EventCommentCreated,
		Payload: commentPayload(comment),
	})

	if len(mock.getCalls()) != 0 {
		t.Error("comment without @mention should not produce a notification")
	}
}

func TestLarkListeners_CommentCreated_IssueMentionOnly_Silent(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	comment := handler.CommentResponse{
		ID:         "comment-1",
		IssueID:    "issue-uuid-1",
		AuthorType: "member",
		AuthorID:   "author-uuid",
		Content:    "Related to [MUL-42](mention://issue/00000000-0000-0000-0000-000000000042)",
	}

	bus.Publish(events.Event{
		Type:    protocol.EventCommentCreated,
		Payload: commentPayload(comment),
	})

	if len(mock.getCalls()) != 0 {
		t.Error("issue-only mention should not produce a notification")
	}
}

func TestLarkListeners_CommentCreated_MapPayload_AlsoWorks(t *testing.T) {
	mock := &mockLarkNotifier{}
	bus := events.New()
	registerLarkListeners(bus, mock, nil)

	bus.Publish(events.Event{
		Type: protocol.EventCommentCreated,
		Payload: map[string]any{
			"comment": map[string]any{
				"issue_id":    "issue-uuid-1",
				"author_type": "agent",
				"author_id":   "agent-uuid",
				"content":     "[@Human](mention://member/dddd-dddd-dddd) I need clarification",
			},
		},
	})

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("map-form comment payload should work; got %d calls", len(calls))
	}
}

// --- registerLarkThreadListeners tests ---

func TestLarkThreadListeners_NilMirror_DoesNotPanic(t *testing.T) {
	bus := events.New()
	registerLarkThreadListeners(bus, nil, nil)
	bus.Publish(events.Event{
		Type: protocol.EventCommentCreated,
		Payload: commentPayload(handler.CommentResponse{
			IssueID: "x", AuthorType: "agent", AuthorID: "a-uuid",
			Content: "[@X](mention://member/eeee-eeee-eeee) hello",
		}),
	})
}

func TestLarkThreadListeners_AgentComment_Mirrored(t *testing.T) {
	mirror := &mockLarkThreadMirror{configured: true}
	bus := events.New()
	registerLarkThreadListeners(bus, mirror, nil)

	comment := handler.CommentResponse{
		IssueID:    "00000000-0000-0000-0000-000000000001",
		AuthorType: "agent",
		AuthorID:   "00000000-0000-0000-0000-000000000002",
		Content:    "I have a question about the requirements.",
	}

	bus.Publish(events.Event{
		Type:    protocol.EventCommentCreated,
		Payload: commentPayload(comment),
	})

	calls := mirror.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 mirror call, got %d", len(calls))
	}
	if calls[0].content != comment.Content {
		t.Error("content mismatch")
	}
}

func TestLarkThreadListeners_MemberComment_NotMirrored(t *testing.T) {
	mirror := &mockLarkThreadMirror{configured: true}
	bus := events.New()
	registerLarkThreadListeners(bus, mirror, nil)

	comment := handler.CommentResponse{
		IssueID:    "00000000-0000-0000-0000-000000000001",
		AuthorType: "member",
		AuthorID:   "00000000-0000-0000-0000-000000000002",
		Content:    "LGTM, merging.",
	}

	bus.Publish(events.Event{
		Type:    protocol.EventCommentCreated,
		Payload: commentPayload(comment),
	})

	if len(mirror.getCalls()) != 0 {
		t.Error("member comment should NOT be mirrored to Lark thread")
	}
}

func TestLarkThreadListeners_LarkThreadSource_NotMirrored(t *testing.T) {
	mirror := &mockLarkThreadMirror{configured: true}
	bus := events.New()
	registerLarkThreadListeners(bus, mirror, nil)

	comment := handler.CommentResponse{
		IssueID:    "00000000-0000-0000-0000-000000000001",
		AuthorType: "agent",
		AuthorID:   "00000000-0000-0000-0000-000000000002",
		Content:    "Agent response",
	}

	bus.Publish(events.Event{
		Type:    protocol.EventCommentCreated,
		Payload: commentPayloadWithSource(comment, "lark_thread"),
	})

	if len(mirror.getCalls()) != 0 {
		t.Error("comment with source=lark_thread must be skipped to prevent echo loop")
	}
}

func TestLarkThreadListeners_NotConfigured_Silent(t *testing.T) {
	mirror := &mockLarkThreadMirror{configured: false}
	bus := events.New()
	registerLarkThreadListeners(bus, mirror, nil)

	comment := handler.CommentResponse{
		IssueID:    "00000000-0000-0000-0000-000000000001",
		AuthorType: "agent",
		AuthorID:   "00000000-0000-0000-0000-000000000002",
		Content:    "Agent comment",
	}

	bus.Publish(events.Event{
		Type:    protocol.EventCommentCreated,
		Payload: commentPayload(comment),
	})

	if len(mirror.getCalls()) != 0 {
		t.Error("unconfigured thread service should silently skip")
	}
}

func TestLarkThreadListeners_InvalidIssueID_Silent(t *testing.T) {
	mirror := &mockLarkThreadMirror{configured: true}
	bus := events.New()
	registerLarkThreadListeners(bus, mirror, nil)

	comment := handler.CommentResponse{
		IssueID:    "not-a-uuid",
		AuthorType: "agent",
		AuthorID:   "00000000-0000-0000-0000-000000000002",
		Content:    "Agent comment",
	}

	bus.Publish(events.Event{
		Type:    protocol.EventCommentCreated,
		Payload: commentPayload(comment),
	})

	if len(mirror.getCalls()) != 0 {
		t.Error("invalid issue UUID should silently skip")
	}
}

// --- extractCommentFields tests ---

func TestExtractCommentFields_StructForm(t *testing.T) {
	c := handler.CommentResponse{
		IssueID:    "issue-1",
		AuthorType: "member",
		AuthorID:   "author-1",
		Content:    "hello",
	}
	issueID, authorType, authorID, content, ok := extractCommentFields(c)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if issueID != "issue-1" || authorType != "member" || authorID != "author-1" || content != "hello" {
		t.Errorf("unexpected fields: %s %s %s %s", issueID, authorType, authorID, content)
	}
}

func TestExtractCommentFields_MapForm(t *testing.T) {
	m := map[string]any{
		"issue_id":    "issue-2",
		"author_type": "agent",
		"author_id":   "agent-2",
		"content":     "world",
	}
	issueID, authorType, authorID, content, ok := extractCommentFields(m)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if issueID != "issue-2" || authorType != "agent" || authorID != "agent-2" || content != "world" {
		t.Errorf("unexpected fields: %s %s %s %s", issueID, authorType, authorID, content)
	}
}

func TestExtractCommentFields_MissingIssueID_Fails(t *testing.T) {
	m := map[string]any{"author_id": "a"}
	_, _, _, _, ok := extractCommentFields(m)
	if ok {
		t.Error("missing issue_id should return ok=false")
	}
}

func TestExtractCommentFields_MissingAuthorID_Fails(t *testing.T) {
	m := map[string]any{"issue_id": "i"}
	_, _, _, _, ok := extractCommentFields(m)
	if ok {
		t.Error("missing author_id should return ok=false")
	}
}

func TestExtractCommentFields_WrongType_Fails(t *testing.T) {
	_, _, _, _, ok := extractCommentFields(42)
	if ok {
		t.Error("non-struct non-map should return ok=false")
	}
}

// --- hasUserMention tests ---

func TestHasUserMention_MemberMention(t *testing.T) {
	if !hasUserMention("Hey [@Alice](mention://member/aaaa-aaaa-aaaa) check this") {
		t.Error("member mention should return true")
	}
}

func TestHasUserMention_AgentMention(t *testing.T) {
	if !hasUserMention("[@Bot](mention://agent/bbbb-bbbb-bbbb) please clarify") {
		t.Error("agent mention should return true")
	}
}

func TestHasUserMention_SquadMention(t *testing.T) {
	if !hasUserMention("[@Team](mention://squad/cccc-cccc-cccc) heads up") {
		t.Error("squad mention should return true")
	}
}

func TestHasUserMention_AllMention(t *testing.T) {
	if !hasUserMention("[@all](mention://all/all) important update") {
		t.Error("@all mention should return true")
	}
}

func TestHasUserMention_IssueMentionOnly(t *testing.T) {
	if hasUserMention("See [MUL-42](mention://issue/00000000-0000-0000-0000-000000000042)") {
		t.Error("issue-only mention should return false")
	}
}

func TestHasUserMention_NoMention(t *testing.T) {
	if hasUserMention("No mentions here") {
		t.Error("no mention should return false")
	}
}

// --- Both listeners on the same bus (integration sanity) ---

// TestLarkListeners_BothRegistered_CommentWithMention_AgentAuthor_BothFire
// documents current behavior: an agent comment with @mention fires BOTH
// NotifyComment (team chat via dispatch) AND MirrorAgentCommentToThread
// (Lark thread reply). Whether the team-chat card is desirable when the
// thread already receives the reply is a product decision — this test
// pins the status quo so any change is intentional.
func TestLarkListeners_BothRegistered_CommentWithMention_AgentAuthor_BothFire(t *testing.T) {
	notifyMock := &mockLarkNotifier{}
	mirrorMock := &mockLarkThreadMirror{configured: true}
	bus := events.New()
	registerLarkListeners(bus, notifyMock, nil)
	registerLarkThreadListeners(bus, mirrorMock, nil)

	comment := handler.CommentResponse{
		IssueID:    "00000000-0000-0000-0000-000000000001",
		AuthorType: "agent",
		AuthorID:   "00000000-0000-0000-0000-000000000002",
		Content:    "[@Human](mention://member/dddd-dddd-dddd) I need help",
	}

	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: "ws-uuid",
		Payload:     commentPayload(comment),
	})

	notifyCalls := notifyMock.getCalls()
	if len(notifyCalls) != 1 {
		t.Errorf("expected 1 notify call, got %d", len(notifyCalls))
	}
	mirrorCalls := mirrorMock.getCalls()
	if len(mirrorCalls) != 1 {
		t.Errorf("expected 1 mirror call, got %d", len(mirrorCalls))
	}
}

func TestLarkListeners_BothRegistered_MemberComment_OnlyNotifyFires(t *testing.T) {
	notifyMock := &mockLarkNotifier{}
	mirrorMock := &mockLarkThreadMirror{configured: true}
	bus := events.New()
	registerLarkListeners(bus, notifyMock, nil)
	registerLarkThreadListeners(bus, mirrorMock, nil)

	comment := handler.CommentResponse{
		IssueID:    "00000000-0000-0000-0000-000000000001",
		AuthorType: "member",
		AuthorID:   "00000000-0000-0000-0000-000000000002",
		Content:    "[@Bot](mention://agent/bbbb-bbbb-bbbb) please run again",
	}

	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: "ws-uuid",
		Payload:     commentPayload(comment),
	})

	notifyCalls := notifyMock.getCalls()
	if len(notifyCalls) != 1 {
		t.Errorf("notify should fire for member comment with mention; got %d", len(notifyCalls))
	}
	mirrorCalls := mirrorMock.getCalls()
	if len(mirrorCalls) != 0 {
		t.Error("mirror should NOT fire for member-authored comment")
	}
}
