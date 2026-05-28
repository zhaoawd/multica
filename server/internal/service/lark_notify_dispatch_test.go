package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// larkSend captures what the mock Lark HTTP server received.
type larkSend struct {
	receiveIDType string
	receiveID     string
	cardJSON      string
}

// mockLarkServer returns an httptest.Server that captures all card sends
// and a function to retrieve the collected calls. The server handles
// tenant_access_token requests and message sends; it returns a canned
// message_id for each send so downstream recordMessageRef exercises.
func mockLarkServer(t *testing.T) (*httptest.Server, func() []larkSend) {
	t.Helper()
	var mu sync.Mutex
	var sends []larkSend

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		if strings.Contains(r.URL.Path, "/im/v1/messages") {
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			sends = append(sends, larkSend{
				receiveIDType: r.URL.Query().Get("receive_id_type"),
				receiveID:     payload["receive_id"],
				cardJSON:      payload["content"],
			})
			mu.Unlock()
			writeJSONResp(w, map[string]any{
				"code": 0,
				"data": map[string]any{"message_id": "om_test_" + payload["receive_id"]},
			})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)

	return srv, func() []larkSend {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]larkSend, len(sends))
		copy(cp, sends)
		return cp
	}
}

// connectTestDB returns a pgxpool connected to the test database.
// Tests that need a real DB call this and get skipped when the DB is
// unavailable — this preserves the existing pure-unit-test story for
// the service package (no package-level TestMain).
func connectTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("DB not available: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("DB not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// dispatchFixture holds IDs created by setupDispatchFixture.
type dispatchFixture struct {
	workspaceID string
	userID      string
	pool        *pgxpool.Pool
}

const (
	dispatchTestWsSlug = "lark-dispatch-test"
	dispatchTestEmail  = "lark-dispatch-test@multica.ai"
	dispatchTestOpenID = "ou_dispatch_test_user"
)

// setupDispatchFixture inserts a workspace, user, member, and binding.
// Call t.Cleanup to remove everything afterwards.
func setupDispatchFixture(t *testing.T, pool *pgxpool.Pool, enabledEvents []string) dispatchFixture {
	t.Helper()
	ctx := context.Background()

	// Best-effort cleanup from prior runs.
	pool.Exec(ctx, `DELETE FROM lark_workspace_binding WHERE workspace_id IN (SELECT id FROM workspace WHERE slug = $1)`, dispatchTestWsSlug)
	pool.Exec(ctx, `DELETE FROM lark_user_link WHERE user_id IN (SELECT id FROM "user" WHERE email = $1)`, dispatchTestEmail)
	pool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, dispatchTestWsSlug)
	pool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, dispatchTestEmail)

	var wsID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, issue_prefix)
		VALUES ('Dispatch Test', $1, 'DPT')
		RETURNING id
	`, dispatchTestWsSlug).Scan(&wsID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ('Dispatch Tester', $1)
		RETURNING id
	`, dispatchTestEmail).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, wsID, userID); err != nil {
		t.Fatalf("create member: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO lark_workspace_binding (workspace_id, chat_id, enabled_events)
		VALUES ($1, 'oc_team_chat', $2)
	`, wsID, enabledEvents); err != nil {
		t.Fatalf("create binding: %v", err)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		pool.Exec(ctx, `DELETE FROM lark_message_ref WHERE workspace_id = $1`, wsID)
		pool.Exec(ctx, `DELETE FROM lark_workspace_binding WHERE workspace_id = $1`, wsID)
		pool.Exec(ctx, `DELETE FROM lark_user_link WHERE user_id = $1`, userID)
		pool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, wsID)
		pool.Exec(ctx, `DELETE FROM "user" WHERE email = $1`, dispatchTestEmail)
	})

	return dispatchFixture{workspaceID: wsID, userID: userID, pool: pool}
}

// linkUserToLark inserts a lark_user_link row for the test user.
func linkUserToLark(t *testing.T, pool *pgxpool.Pool, userID, openID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO lark_user_link (user_id, lark_open_id)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET lark_open_id = $2
	`, userID, openID); err != nil {
		t.Fatalf("link user to lark: %v", err)
	}
}

// setUserPrefs updates the prefs JSONB column on lark_user_link.
func setUserPrefs(t *testing.T, pool *pgxpool.Pool, userID string, prefs LarkUserPref) {
	t.Helper()
	prefsJSON, err := json.Marshal(prefs)
	if err != nil {
		t.Fatalf("marshal prefs: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE lark_user_link SET prefs = $2 WHERE user_id = $1
	`, userID, prefsJSON); err != nil {
		t.Fatalf("set user prefs: %v", err)
	}
}

// newNotifyWithDB constructs a LarkNotify backed by a real DB and a
// mock Lark HTTP server. Workers are NOT started — enqueue() processes
// jobs inline, which is what we want for synchronous assertions.
func newNotifyWithDB(t *testing.T, pool *pgxpool.Pool, larkSrv *httptest.Server) *LarkNotify {
	t.Helper()
	cfg := fullCfg()
	client := NewLarkClient(cfg)
	SetAPIBaseForTest(client, larkSrv.URL)
	return &LarkNotify{
		cfg:         cfg,
		client:      client,
		queries:     db.New(pool),
		frontend:    "http://localhost:3000",
		log:         testLogger(),
		jobs:        make(chan larkJob, 8),
		stopCh:      make(chan struct{}),
		sendTimeout: 5 * time.Second,
	}
}

// ── dispatchRouted via NotifyIssueCreated ────────────────────────────────

func TestDispatchRouted_NoBinding_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	ctx := context.Background()

	// Create a workspace WITHOUT a binding.
	pool.Exec(ctx, `DELETE FROM workspace WHERE slug = 'no-binding-test'`)
	var wsID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, issue_prefix)
		VALUES ('No Binding', 'no-binding-test', 'NBT')
		RETURNING id
	`).Scan(&wsID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, wsID)
	})

	n := newNotifyWithDB(t, pool, srv)
	n.NotifyIssueCreated(ctx, wsID, IssueInfo{
		IssueID: "00000000-0000-0000-0000-000000000001",
		Title:   "test",
	}, false, "", false, false, "alice")

	if len(getSends()) != 0 {
		t.Error("no binding → expected no sends")
	}
}

func TestDispatchRouted_EventNotEnabled_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)

	// Binding exists but only enables issue:updated, not issue:created.
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueUpdated})
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyIssueCreated(context.Background(), fix.workspaceID, IssueInfo{
		IssueID: "00000000-0000-0000-0000-000000000001",
		Title:   "test",
	}, false, "", false, false, "alice")

	if len(getSends()) != 0 {
		t.Error("event not in enabled_events → expected no sends")
	}
}

func TestDispatchRouted_UnassignedIssue_TeamChat(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyIssueCreated(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-1",
		Title:         "Unassigned issue",
		WorkspaceSlug: dispatchTestWsSlug,
	}, false, "", false, false, "alice")

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 send for unassigned issue; got %d", len(sends))
	}
	if sends[0].receiveIDType != "chat_id" {
		t.Errorf("expected chat_id send; got receive_id_type=%s", sends[0].receiveIDType)
	}
	if sends[0].receiveID != "oc_team_chat" {
		t.Errorf("expected oc_team_chat; got %s", sends[0].receiveID)
	}
}

func TestDispatchRouted_AssigneeWithLarkLink_DM(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyIssueCreated(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-2",
		Title:         "Assigned issue",
		WorkspaceSlug: dispatchTestWsSlug,
	}, true, fix.userID, false, false, "alice")

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 send for assigned issue with linked user; got %d", len(sends))
	}
	if sends[0].receiveIDType != "open_id" {
		t.Errorf("expected DM (open_id); got receive_id_type=%s", sends[0].receiveIDType)
	}
	if sends[0].receiveID != dispatchTestOpenID {
		t.Errorf("expected %s; got %s", dispatchTestOpenID, sends[0].receiveID)
	}
}

// TestDispatchRouted_AssigneeWithoutLarkLink_Silent pins the design-spec
// behavior (LARK_INTEGRATION_TEST.md §方向一/3): when a DM route is
// selected but the assignee has no lark_user_link, the notification is
// dropped silently — not broadcast to the team chat. The absence of a
// link IS the opt-out; falling back to public would surface intended-
// personal signals to everyone in the group.
func TestDispatchRouted_AssigneeWithoutLarkLink_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	// No linkUserToLark — user is NOT linked.
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyIssueCreated(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-3",
		Title:         "Assigned but unlinked",
		WorkspaceSlug: dispatchTestWsSlug,
	}, true, fix.userID, false, false, "alice")

	if sends := getSends(); len(sends) != 0 {
		t.Errorf("unlinked assignee should yield 0 sends (silent degrade); got %d", len(sends))
	}
}

func TestDispatchRouted_AssignedDMPrefOff_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	setUserPrefs(t, pool, fix.userID, LarkUserPref{
		AssignedDM:           false,
		AgentClarificationDM: true,
	})
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyIssueCreated(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-4",
		Title:         "Assigned but DM pref off",
		WorkspaceSlug: dispatchTestWsSlug,
	}, true, fix.userID, false, false, "alice")

	if len(getSends()) != 0 {
		t.Error("AssignedDM=false → routing returns empty channels → expected no sends")
	}
}

func TestDispatchRouted_WorkspaceAgent_TeamChat(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	n := newNotifyWithDB(t, pool, srv)

	// assigneeIsWorkspaceAgent=true routes to team chat regardless of user link.
	n.NotifyIssueCreated(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-5",
		Title:         "Cloud agent assigned",
		WorkspaceSlug: dispatchTestWsSlug,
	}, true, "", false, true, "alice")

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 send for workspace agent; got %d", len(sends))
	}
	if sends[0].receiveIDType != "chat_id" {
		t.Errorf("workspace agent → expected team chat; got %s", sends[0].receiveIDType)
	}
}

// ── dispatchRouted via NotifyIssueAssigned ───────────────────────────────

func TestDispatchRouted_IssueAssigned_LinkedUser_DM(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueUpdated})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyIssueAssigned(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-6",
		Title:         "Reassigned issue",
		WorkspaceSlug: dispatchTestWsSlug,
	}, "Dispatch Tester", fix.userID, false)

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 DM send; got %d", len(sends))
	}
	if sends[0].receiveIDType != "open_id" {
		t.Errorf("expected DM (open_id); got %s", sends[0].receiveIDType)
	}
	if sends[0].receiveID != dispatchTestOpenID {
		t.Errorf("expected %s; got %s", dispatchTestOpenID, sends[0].receiveID)
	}
}

// TestDispatchRouted_IssueAssigned_UnlinkedUser_Silent — assignment to
// an unlinked user yields no card. See the comment on
// TestDispatchRouted_AssigneeWithoutLarkLink_Silent for the rationale.
func TestDispatchRouted_IssueAssigned_UnlinkedUser_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueUpdated})
	// No linkUserToLark.
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyIssueAssigned(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-7",
		Title:         "Reassigned to unlinked user",
		WorkspaceSlug: dispatchTestWsSlug,
	}, "Dispatch Tester", fix.userID, false)

	if sends := getSends(); len(sends) != 0 {
		t.Errorf("unlinked user should yield 0 sends; got %d", len(sends))
	}
}

// ── dispatch (non-routed) via NotifyComment ──────────────────────────────

func TestDispatch_CommentNotification_TeamChat(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventCommentCreated})
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyComment(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		Identifier:    "DPT-8",
		Title:         "Comment issue",
		WorkspaceSlug: dispatchTestWsSlug,
	}, "alice", "Need review please")

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 team chat send; got %d", len(sends))
	}
	if sends[0].receiveIDType != "chat_id" {
		t.Errorf("dispatch() always goes to team chat; got %s", sends[0].receiveIDType)
	}
}

// ── Task completed/failed via dispatchRouted ─────────────────────────────

func TestDispatchRouted_TaskCompleted_PrefOn_DM(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventTaskCompleted})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	setUserPrefs(t, pool, fix.userID, LarkUserPref{TaskCompletedDM: true})
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyTaskCompleted(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		Identifier:    "DPT-9",
		Title:         "Done task",
		WorkspaceSlug: dispatchTestWsSlug,
	}, true, fix.userID)

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 DM send; got %d", len(sends))
	}
	if sends[0].receiveIDType != "open_id" {
		t.Errorf("TaskCompletedDM=true → expected DM; got %s", sends[0].receiveIDType)
	}
	if sends[0].receiveID != dispatchTestOpenID {
		t.Errorf("expected %s; got %s", dispatchTestOpenID, sends[0].receiveID)
	}
}

func TestDispatchRouted_TaskCompleted_PrefOff_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventTaskCompleted})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	// Default prefs have TaskCompletedDM=false.
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyTaskCompleted(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		Identifier:    "DPT-10",
		Title:         "Done task silent",
		WorkspaceSlug: dispatchTestWsSlug,
	}, true, fix.userID)

	if len(getSends()) != 0 {
		t.Error("TaskCompletedDM=false (default) → expected no sends")
	}
}

func TestDispatchRouted_TaskCompleted_NoAssignee_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventTaskCompleted})
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyTaskCompleted(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		Identifier:    "DPT-11",
		Title:         "Orphan task",
		WorkspaceSlug: dispatchTestWsSlug,
	}, false, "")

	if len(getSends()) != 0 {
		t.Error("task:completed with no assignee → silent per §6.1")
	}
}

func TestDispatchRouted_TaskFailed_NoAssignee_PublicBlocker(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventTaskFailed})
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyTaskFailed(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		Identifier:    "DPT-12",
		Title:         "Failed orphan",
		WorkspaceSlug: dispatchTestWsSlug,
	}, "runtime offline", false, "")

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("no-assignee failure → team chat public blocker; got %d sends", len(sends))
	}
	if sends[0].receiveIDType != "chat_id" {
		t.Errorf("expected team chat; got %s", sends[0].receiveIDType)
	}
}

func TestDispatchRouted_TaskFailed_PrefOn_DM(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventTaskFailed})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	setUserPrefs(t, pool, fix.userID, LarkUserPref{TaskFailedDM: true})
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyTaskFailed(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		Identifier:    "DPT-13",
		Title:         "Failed task",
		WorkspaceSlug: dispatchTestWsSlug,
	}, "agent crashed", true, fix.userID)

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("TaskFailedDM=true → expected 1 DM; got %d", len(sends))
	}
	if sends[0].receiveIDType != "open_id" {
		t.Errorf("expected DM; got %s", sends[0].receiveIDType)
	}
}

func TestDispatchRouted_TaskFailed_PrefOff_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventTaskFailed})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	// Default prefs have TaskFailedDM=false.
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyTaskFailed(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		Identifier:    "DPT-14",
		Title:         "Failed silent",
		WorkspaceSlug: dispatchTestWsSlug,
	}, "error", true, fix.userID)

	if len(getSends()) != 0 {
		t.Error("TaskFailedDM=false (default) + has assignee → silent")
	}
}

func TestDispatchRouted_TaskFailed_UnlinkedAssignee_Silent(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventTaskFailed})
	// User has no lark_user_link → resolveLarkUserPref returns default
	// prefs (TaskFailedDM=false) → routing returns no channels → silent.
	n := newNotifyWithDB(t, pool, srv)

	n.NotifyTaskFailed(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       "00000000-0000-0000-0000-000000000001",
		Identifier:    "DPT-15",
		Title:         "Failed unlinked",
		WorkspaceSlug: dispatchTestWsSlug,
	}, "error", true, fix.userID)

	// User has no lark_user_link → resolveLarkUserPref returns default
	// (TaskFailedDM=false) → routing returns empty channels → silent.
	if len(getSends()) != 0 {
		t.Error("unlinked user with default prefs → TaskFailedDM=false → silent")
	}
}

// ── Message ref recording ────────────────────────────────────────────────

func TestDispatchRouted_RecordsMessageRef(t *testing.T) {
	pool := connectTestDB(t)
	srv, getSends := mockLarkServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	n := newNotifyWithDB(t, pool, srv)

	// Create a real issue so message_ref FK doesn't fail.
	var issueID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'Ref test', 'todo', 'medium', 'member', $2, 99)
		RETURNING id
	`, fix.workspaceID, fix.userID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM lark_message_ref WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	n.NotifyIssueCreated(context.Background(), fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-99",
		Title:         "Ref test",
		WorkspaceSlug: dispatchTestWsSlug,
	}, false, "", false, false, "alice")

	sends := getSends()
	if len(sends) != 1 {
		t.Fatalf("expected 1 send; got %d", len(sends))
	}

	// Verify a message_ref was recorded.
	var refCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM lark_message_ref
		WHERE issue_id = $1 AND status = 'active'
	`, issueID).Scan(&refCount); err != nil {
		t.Fatalf("query message refs: %v", err)
	}
	if refCount != 1 {
		t.Errorf("expected 1 active message ref; got %d", refCount)
	}
}

// ── PatchIssueTerminalCards ──────────────────────────────────────────────

func TestPatchIssueTerminalCards_FinalizesRef(t *testing.T) {
	pool := connectTestDB(t)
	ctx := context.Background()

	// We need a real send + patch cycle. Set up fixture with a mock server
	// that handles both sends and patches.
	var mu sync.Mutex
	var patchCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		if r.Method == http.MethodPatch {
			mu.Lock()
			patchCount++
			mu.Unlock()
			writeJSONResp(w, map[string]any{"code": 0})
			return
		}
		// Regular send
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		writeJSONResp(w, map[string]any{
			"code": 0,
			"data": map[string]any{"message_id": "om_patch_test"},
		})
	}))
	t.Cleanup(srv.Close)

	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated, protocol.EventIssueUpdated})
	n := newNotifyWithDB(t, pool, srv)

	var issueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'Patch test', 'todo', 'medium', 'member', $2, 100)
		RETURNING id
	`, fix.workspaceID, fix.userID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM lark_message_ref WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	// Step 1: Send an initial card (creates an active message_ref).
	n.NotifyIssueCreated(ctx, fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-100",
		Title:         "Patch test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "todo",
	}, false, "", false, false, "alice")

	// Verify active ref exists.
	var activeCount int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'active'`, issueID).Scan(&activeCount)
	if activeCount != 1 {
		t.Fatalf("expected 1 active ref after initial send; got %d", activeCount)
	}

	// Step 2: Patch terminal cards.
	n.PatchIssueTerminalCards(ctx, fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-100",
		Title:         "Patch test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "done",
	})

	mu.Lock()
	patches := patchCount
	mu.Unlock()
	if patches != 1 {
		t.Errorf("expected 1 PATCH request; got %d", patches)
	}

	// Verify ref is finalized.
	var finalizedCount int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'finalized'`, issueID).Scan(&finalizedCount)
	if finalizedCount != 1 {
		t.Errorf("expected 1 finalized ref after terminal patch; got %d", finalizedCount)
	}
}

// ── Message ref lifecycle (#4 in LARK_INTEGRATION_TEST.md) ──────────────
//
// These tests pin the active → finalized state machine and the upsert
// idempotency invariants of lark_message_ref:
//   - Same (issue, stage, channel, target) re-send updates in place,
//     never creates a duplicate active row.
//   - PatchIssueTerminalCards is a no-op when status is non-terminal.
//   - PatchIssueTerminalCards is idempotent: a second call after the
//     first finalize produces 0 patches (no active refs left).
//   - Multiple stages on the same issue → all get patched on terminal.
//   - The partial unique index allows a new active row after finalize,
//     so resending the same card kind post-terminal still works.

// sendAndPatchServer is a mock Lark API that handles tenant_access_token,
// POST /im/v1/messages (send), and PATCH for card updates, tracking
// counts for each. Used by ref-lifecycle tests that need to drive a
// full send+patch cycle.
func sendAndPatchServer(t *testing.T) (*httptest.Server, func() (sends int, patches int)) {
	t.Helper()
	var mu sync.Mutex
	var sends, patches int
	var sendCounter int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tenant_access_token") {
			writeJSONResp(w, map[string]any{"code": 0, "tenant_access_token": "tk", "expire": 7200})
			return
		}
		if r.Method == http.MethodPatch {
			mu.Lock()
			patches++
			mu.Unlock()
			writeJSONResp(w, map[string]any{"code": 0})
			return
		}
		if strings.Contains(r.URL.Path, "/im/v1/messages") {
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			sends++
			sendCounter++
			id := sendCounter
			mu.Unlock()
			// Return a unique message_id per send so each ref gets a
			// distinct row when the upsert partial index permits insert.
			writeJSONResp(w, map[string]any{
				"code": 0,
				"data": map[string]any{"message_id": "om_test_" + payload["receive_id"] + "_" + itoa(id)},
			})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return srv, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return sends, patches
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// createRefTestIssue inserts an issue suitable for ref-lifecycle tests
// and registers cleanup for the issue and any refs/links it produces.
func createRefTestIssue(t *testing.T, pool *pgxpool.Pool, fix dispatchFixture, number int, title string) string {
	t.Helper()
	ctx := context.Background()
	var issueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, number)
		VALUES ($1, $2, 'todo', 'medium', 'member', $3, $4)
		RETURNING id
	`, fix.workspaceID, title, fix.userID, number).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM lark_message_ref WHERE issue_id = $1`, issueID)
		pool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})
	return issueID
}

// TestPatchIssueTerminalCards_NonTerminalStatus_NoOp — early-return when
// the new issue status is not terminal (e.g. todo → in_progress). No
// PATCH is issued; the active ref remains active.
func TestPatchIssueTerminalCards_NonTerminalStatus_NoOp(t *testing.T) {
	pool := connectTestDB(t)
	srv, counts := sendAndPatchServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated, protocol.EventIssueUpdated})
	n := newNotifyWithDB(t, pool, srv)

	issueID := createRefTestIssue(t, pool, fix, 200, "Non-terminal test")
	ctx := context.Background()

	n.NotifyIssueCreated(ctx, fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-200",
		Title:         "Non-terminal test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "todo",
	}, false, "", false, false, "alice")

	// Call patch with a non-terminal status.
	n.PatchIssueTerminalCards(ctx, fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-200",
		Title:         "Non-terminal test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "in_progress",
	})

	_, patches := counts()
	if patches != 0 {
		t.Errorf("non-terminal status should not trigger PATCH; got %d", patches)
	}
	var activeCount int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'active'`, issueID).Scan(&activeCount)
	if activeCount != 1 {
		t.Errorf("ref should still be active; got %d active rows", activeCount)
	}
}

// TestPatchIssueTerminalCards_NoActiveRefs_NoOp — no card has been sent
// for this issue, so terminal patch finds nothing to update and emits
// no HTTP requests.
func TestPatchIssueTerminalCards_NoActiveRefs_NoOp(t *testing.T) {
	pool := connectTestDB(t)
	srv, counts := sendAndPatchServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueUpdated})
	n := newNotifyWithDB(t, pool, srv)

	issueID := createRefTestIssue(t, pool, fix, 201, "No refs test")
	ctx := context.Background()

	n.PatchIssueTerminalCards(ctx, fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-201",
		Title:         "No refs test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "done",
	})

	sends, patches := counts()
	if sends != 0 || patches != 0 {
		t.Errorf("no active refs should produce no HTTP; got sends=%d patches=%d", sends, patches)
	}
}

// TestPatchIssueTerminalCards_IdempotentAfterFinalize — pin the contract
// that a second terminal patch after the first is a no-op. The
// FinalizeLarkMessageRef SQL filters WHERE status='active', so even if
// PatchIssueTerminalCards were called for the same job, the partial
// index would no longer match. This test verifies via the higher-level
// API that the workflow holds end-to-end.
func TestPatchIssueTerminalCards_IdempotentAfterFinalize(t *testing.T) {
	pool := connectTestDB(t)
	srv, counts := sendAndPatchServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated, protocol.EventIssueUpdated})
	n := newNotifyWithDB(t, pool, srv)

	issueID := createRefTestIssue(t, pool, fix, 202, "Idempotent patch test")
	ctx := context.Background()

	n.NotifyIssueCreated(ctx, fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-202",
		Title:         "Idempotent patch test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "todo",
	}, false, "", false, false, "alice")

	terminal := IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-202",
		Title:         "Idempotent patch test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "done",
	}
	n.PatchIssueTerminalCards(ctx, fix.workspaceID, terminal)
	// Second call: no active refs remain, so PATCH count must not advance.
	n.PatchIssueTerminalCards(ctx, fix.workspaceID, terminal)

	_, patches := counts()
	if patches != 1 {
		t.Errorf("idempotent terminal patch should issue exactly 1 PATCH; got %d", patches)
	}
	var activeCount, finalizedCount int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'active'`, issueID).Scan(&activeCount)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'finalized'`, issueID).Scan(&finalizedCount)
	if activeCount != 0 {
		t.Errorf("expected 0 active refs after finalize; got %d", activeCount)
	}
	if finalizedCount != 1 {
		t.Errorf("expected exactly 1 finalized ref; got %d", finalizedCount)
	}
}

// TestUpsertMessageRef_SameKey_UpdatesInPlace — sending the same card
// kind to the same channel/target twice must NOT produce two active
// refs. The partial unique index on (issue_id, stage_or_event, channel,
// target_id) WHERE status='active' makes this an in-place upsert with
// version increment.
func TestUpsertMessageRef_SameKey_UpdatesInPlace(t *testing.T) {
	pool := connectTestDB(t)
	srv, counts := sendAndPatchServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	n := newNotifyWithDB(t, pool, srv)

	issueID := createRefTestIssue(t, pool, fix, 203, "Upsert in place test")
	ctx := context.Background()

	info := IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-203",
		Title:         "Upsert in place test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "todo",
	}
	n.NotifyIssueCreated(ctx, fix.workspaceID, info, false, "", false, false, "alice")
	n.NotifyIssueCreated(ctx, fix.workspaceID, info, false, "", false, false, "alice")

	sends, _ := counts()
	if sends != 2 {
		t.Errorf("expected 2 send HTTP calls (one per Notify); got %d", sends)
	}

	var totalCount, activeCount, version int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1`, issueID).Scan(&totalCount)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'active'`, issueID).Scan(&activeCount)
	pool.QueryRow(ctx, `SELECT version FROM lark_message_ref WHERE issue_id = $1 AND status = 'active'`, issueID).Scan(&version)
	if totalCount != 1 {
		t.Errorf("expected exactly 1 ref row (in-place upsert); got %d total rows", totalCount)
	}
	if activeCount != 1 {
		t.Errorf("expected 1 active ref; got %d", activeCount)
	}
	if version < 1 {
		t.Errorf("expected version >= 1 after second send (upsert path); got %d", version)
	}
}

// TestPatchIssueTerminalCards_MultipleActiveRefs_AllPatched — when an
// issue has accumulated multiple active cards across different stages
// (e.g. claim card + public-blocker failure card), reaching terminal
// status must patch and finalize ALL of them, not just one.
func TestPatchIssueTerminalCards_MultipleActiveRefs_AllPatched(t *testing.T) {
	pool := connectTestDB(t)
	srv, counts := sendAndPatchServer(t)
	fix := setupDispatchFixture(t, pool, []string{
		protocol.EventIssueCreated,
		protocol.EventIssueUpdated,
		protocol.EventTaskFailed,
	})
	n := newNotifyWithDB(t, pool, srv)

	issueID := createRefTestIssue(t, pool, fix, 204, "Multi-stage test")
	ctx := context.Background()

	info := IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-204",
		Title:         "Multi-stage test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "todo",
	}
	// Card 1: claim card from NotifyIssueCreated (stage=claim_card).
	n.NotifyIssueCreated(ctx, fix.workspaceID, info, false, "", false, false, "alice")
	// Card 2: public-blocker card from NotifyTaskFailed with no assignee
	// (stage=public_blocker). Different stage_or_event → distinct active
	// row under the partial unique index.
	n.NotifyTaskFailed(ctx, fix.workspaceID, info, "runtime offline", false, "")

	var preActive int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'active'`, issueID).Scan(&preActive)
	if preActive != 2 {
		t.Fatalf("expected 2 active refs (claim + public_blocker); got %d", preActive)
	}

	n.PatchIssueTerminalCards(ctx, fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-204",
		Title:         "Multi-stage test",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "done",
	})

	_, patches := counts()
	if patches != 2 {
		t.Errorf("expected 2 PATCH requests for 2 active refs; got %d", patches)
	}
	var activeAfter, finalizedAfter int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'active'`, issueID).Scan(&activeAfter)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'finalized'`, issueID).Scan(&finalizedAfter)
	if activeAfter != 0 {
		t.Errorf("expected 0 active refs after terminal patch; got %d", activeAfter)
	}
	if finalizedAfter != 2 {
		t.Errorf("expected 2 finalized refs; got %d", finalizedAfter)
	}
}

// TestUpsertMessageRef_AfterFinalize_NewActiveRowCreated — the partial
// unique index (issue_id, stage_or_event, channel, target_id) WHERE
// status='active' permits a new active row once the previous one has
// been finalized. This is the post-terminal recovery path: if the same
// card kind is somehow re-sent after the issue closed, a fresh ref is
// recorded rather than the upsert silently colliding.
func TestUpsertMessageRef_AfterFinalize_NewActiveRowCreated(t *testing.T) {
	pool := connectTestDB(t)
	srv, counts := sendAndPatchServer(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated, protocol.EventIssueUpdated})
	n := newNotifyWithDB(t, pool, srv)

	issueID := createRefTestIssue(t, pool, fix, 205, "Resend after finalize")
	ctx := context.Background()

	info := IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-205",
		Title:         "Resend after finalize",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "todo",
	}
	n.NotifyIssueCreated(ctx, fix.workspaceID, info, false, "", false, false, "alice")

	n.PatchIssueTerminalCards(ctx, fix.workspaceID, IssueInfo{
		IssueID:       issueID,
		WorkspaceID:   fix.workspaceID,
		Identifier:    "DPT-205",
		Title:         "Resend after finalize",
		WorkspaceSlug: dispatchTestWsSlug,
		Status:        "done",
	})

	// Now resend the same kind of card. The partial unique index ignores
	// the finalized row, so this must INSERT a brand-new active row.
	n.NotifyIssueCreated(ctx, fix.workspaceID, info, false, "", false, false, "alice")

	sends, patches := counts()
	if sends != 2 {
		t.Errorf("expected 2 sends; got %d", sends)
	}
	if patches != 1 {
		t.Errorf("expected 1 patch; got %d", patches)
	}

	var totalRows, activeCount, finalizedCount int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1`, issueID).Scan(&totalRows)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'active'`, issueID).Scan(&activeCount)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM lark_message_ref WHERE issue_id = $1 AND status = 'finalized'`, issueID).Scan(&finalizedCount)
	if totalRows != 2 {
		t.Errorf("expected 2 total rows (1 finalized + 1 new active); got %d", totalRows)
	}
	if activeCount != 1 {
		t.Errorf("expected exactly 1 active ref after resend; got %d", activeCount)
	}
	if finalizedCount != 1 {
		t.Errorf("expected exactly 1 finalized ref preserved; got %d", finalizedCount)
	}
}

// dummyLarkServer returns a properly-cleaned-up httptest.Server that
// responds 404 to everything. Used by tests that need a LarkNotify
// instance but don't care about HTTP sends.
func dummyLarkServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv
}

// ── Pref resolution edge cases ──────────────────────────────────────────

func TestResolveLarkUserPref_NoLink_ReturnsDefault(t *testing.T) {
	pool := connectTestDB(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	n := newNotifyWithDB(t, pool, dummyLarkServer(t))

	pref := n.resolveLarkUserPref(context.Background(), fix.userID)
	def := DefaultLarkUserPref()
	if pref != def {
		t.Errorf("unlinked user should get default pref; got %+v", pref)
	}
}

func TestResolveLarkUserPref_LinkedWithCustomPrefs(t *testing.T) {
	pool := connectTestDB(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	setUserPrefs(t, pool, fix.userID, LarkUserPref{
		AssignedDM:           false,
		AgentClarificationDM: false,
		TaskFailedDM:         true,
	})
	n := newNotifyWithDB(t, pool, dummyLarkServer(t))

	pref := n.resolveLarkUserPref(context.Background(), fix.userID)
	if pref.AssignedDM {
		t.Error("expected AssignedDM=false")
	}
	if pref.AgentClarificationDM {
		t.Error("expected AgentClarificationDM=false")
	}
	if !pref.TaskFailedDM {
		t.Error("expected TaskFailedDM=true")
	}
}

func TestResolveLarkUserPref_MalformedJSON_ReturnsDefault(t *testing.T) {
	pool := connectTestDB(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	// Write garbage JSON into the prefs column.
	if _, err := pool.Exec(context.Background(),
		`UPDATE lark_user_link SET prefs = '{"broken'::jsonb WHERE user_id = $1`, fix.userID,
	); err != nil {
		// Postgres rejects invalid JSON at the column level (jsonb type),
		// so we write a valid-JSON-but-wrong-shape value instead.
		pool.Exec(context.Background(),
			`UPDATE lark_user_link SET prefs = '"just a string"'::jsonb WHERE user_id = $1`, fix.userID)
	}
	n := newNotifyWithDB(t, pool, dummyLarkServer(t))

	pref := n.resolveLarkUserPref(context.Background(), fix.userID)
	if pref != DefaultLarkUserPref() {
		t.Errorf("malformed prefs should fall back to default; got %+v", pref)
	}
}

func TestResolveLarkUserPref_EmptyUserID_ReturnsDefault(t *testing.T) {
	pool := connectTestDB(t)
	n := newNotifyWithDB(t, pool, dummyLarkServer(t))

	pref := n.resolveLarkUserPref(context.Background(), "")
	if pref != DefaultLarkUserPref() {
		t.Errorf("empty userID should return default; got %+v", pref)
	}
}

func TestResolveLarkUserPref_InvalidUUID_ReturnsDefault(t *testing.T) {
	pool := connectTestDB(t)
	n := newNotifyWithDB(t, pool, dummyLarkServer(t))

	pref := n.resolveLarkUserPref(context.Background(), "not-a-uuid")
	if pref != DefaultLarkUserPref() {
		t.Errorf("invalid UUID should return default; got %+v", pref)
	}
}

// ── open_id resolution edge cases ───────────────────────────────────────

func TestResolveAssigneeLarkOpenID_LinkedUser(t *testing.T) {
	pool := connectTestDB(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	linkUserToLark(t, pool, fix.userID, dispatchTestOpenID)
	n := newNotifyWithDB(t, pool, dummyLarkServer(t))

	openID := n.resolveAssigneeLarkOpenID(context.Background(), fix.userID)
	if openID != dispatchTestOpenID {
		t.Errorf("expected %s; got %s", dispatchTestOpenID, openID)
	}
}

func TestResolveAssigneeLarkOpenID_UnlinkedUser(t *testing.T) {
	pool := connectTestDB(t)
	fix := setupDispatchFixture(t, pool, []string{protocol.EventIssueCreated})
	n := newNotifyWithDB(t, pool, dummyLarkServer(t))

	openID := n.resolveAssigneeLarkOpenID(context.Background(), fix.userID)
	if openID != "" {
		t.Errorf("unlinked user should return empty; got %s", openID)
	}
}

func TestResolveAssigneeLarkOpenID_EmptyUserID(t *testing.T) {
	pool := connectTestDB(t)
	n := newNotifyWithDB(t, pool, dummyLarkServer(t))

	if openID := n.resolveAssigneeLarkOpenID(context.Background(), ""); openID != "" {
		t.Errorf("empty userID should return empty; got %s", openID)
	}
}
