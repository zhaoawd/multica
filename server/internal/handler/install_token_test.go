package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
)

func createHandlerTestMember(t *testing.T, role string) string {
	t.Helper()
	ctx := context.Background()
	email := "handler-member-" + uuid.NewString() + "@multica.test"

	var userID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ('Handler Test Member', $1)
		RETURNING id
	`, email).Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, $3)
	`, testWorkspaceID, userID, role); err != nil {
		t.Fatalf("create member: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, userID)
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return userID
}

func TestInstallTokenNonAdminCannotTakeOverExistingDaemonID(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	ownerID := createHandlerTestMember(t, "member")
	attackerID := createHandlerTestMember(t, "member")
	daemonID := "takeover-" + uuid.NewString()
	provider := "claude"

	var runtimeID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status,
			device_info, metadata, owner_id, last_seen_at
		)
		VALUES ($1, $2, 'victim-runtime', 'local', $3, 'online', '', '{}'::jsonb, $4, now())
		RETURNING id
	`, testWorkspaceID, daemonID, provider, ownerID).Scan(&runtimeID); err != nil {
		t.Fatalf("seed victim runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	var agentID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, runtime_mode, runtime_config, runtime_id,
			visibility, max_concurrent_tasks, owner_id
		)
		VALUES ($1, $2, 'local', '{}'::jsonb, $3, 'workspace', 1, $4)
		RETURNING id
	`, testWorkspaceID, "victim-agent-"+uuid.NewString(), runtimeID, ownerID).Scan(&agentID); err != nil {
		t.Fatalf("seed victim agent: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, agentID)
	})

	var issueID string
	if err := testPool.QueryRow(ctx, `
		WITH next_issue AS (
			SELECT COALESCE(MAX(number), 0) + 1 AS number FROM issue WHERE workspace_id = $1
		)
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		SELECT $1, 'install token takeover', 'todo', 'medium', $2, 'member', number, 0 FROM next_issue
		RETURNING id
	`, testWorkspaceID, ownerID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, status, runtime_id)
		VALUES ($1, $2, 'running', $3)
		RETURNING id
	`, agentID, issueID, runtimeID).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID)
	})

	mintW := httptest.NewRecorder()
	testHandler.MintInstallToken(mintW, newRequestAs(attackerID, http.MethodPost, "/api/install-tokens", nil))
	if mintW.Code != http.StatusCreated {
		t.Fatalf("MintInstallToken: expected 201, got %d: %s", mintW.Code, mintW.Body.String())
	}
	var mintResp MintInstallTokenResponse
	if err := json.NewDecoder(mintW.Body).Decode(&mintResp); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}

	exchangeW := httptest.NewRecorder()
	testHandler.ExchangeInstallToken(exchangeW, newRequest(http.MethodPost, "/api/install-tokens/exchange", map[string]any{
		"token":     mintResp.Token,
		"daemon_id": daemonID,
	}))
	if exchangeW.Code != http.StatusForbidden {
		t.Fatalf("ExchangeInstallToken takeover: expected 403, got %d: %s", exchangeW.Code, exchangeW.Body.String())
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM install_token WHERE token_hash = $1`, auth.HashToken(mintResp.Token))
	})

	var usedAt pgtype.Timestamptz
	if err := testPool.QueryRow(ctx, `SELECT used_at FROM install_token WHERE token_hash = $1`, auth.HashToken(mintResp.Token)).Scan(&usedAt); err != nil {
		t.Fatalf("read install token used_at: %v", err)
	}
	if usedAt.Valid {
		t.Fatal("rejected takeover exchange consumed the install token")
	}

	var daemonTokenCount int
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM daemon_token
		WHERE workspace_id = $1 AND daemon_id = $2 AND created_by_user_id = $3
	`, testWorkspaceID, daemonID, attackerID).Scan(&daemonTokenCount); err != nil {
		t.Fatalf("count daemon tokens: %v", err)
	}
	if daemonTokenCount != 0 {
		t.Fatalf("expected no attacker daemon token, got %d", daemonTokenCount)
	}

	tokenHash := "takeover-mdt-" + uuid.NewString()
	if _, err := testPool.Exec(ctx, `
		INSERT INTO daemon_token (
			token_hash, workspace_id, daemon_id, expires_at,
			created_by_user_id, install_source
		)
		VALUES ($1, $2, $3, now() + interval '1 hour', $4, 'script')
	`, tokenHash, testWorkspaceID, daemonID, attackerID); err != nil {
		t.Fatalf("seed attacker daemon token: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_token WHERE token_hash = $1`, tokenHash)
	})

	registerW := httptest.NewRecorder()
	testHandler.DaemonRegister(registerW, newDaemonTokenRequest(http.MethodPost, "/api/daemon/register", map[string]any{
		"workspace_id": testWorkspaceID,
		"daemon_id":    daemonID,
		"device_name":  "attacker-machine",
		"runtimes": []map[string]any{
			{"name": "attacker-runtime", "type": provider, "version": "1.0.0", "status": "online"},
		},
	}, testWorkspaceID, daemonID))
	if registerW.Code != http.StatusForbidden {
		t.Fatalf("DaemonRegister takeover: expected 403, got %d: %s", registerW.Code, registerW.Body.String())
	}

	var runtimeIDAfter, ownerIDAfter string
	if err := testPool.QueryRow(ctx, `
		SELECT id, owner_id FROM agent_runtime
		WHERE workspace_id = $1 AND daemon_id = $2 AND provider = $3
	`, testWorkspaceID, daemonID, provider).Scan(&runtimeIDAfter, &ownerIDAfter); err != nil {
		t.Fatalf("read runtime after rejected register: %v", err)
	}
	if runtimeIDAfter != runtimeID {
		t.Fatalf("runtime id changed: got %s want %s", runtimeIDAfter, runtimeID)
	}
	if ownerIDAfter != ownerID {
		t.Fatalf("runtime owner changed: got %s want %s", ownerIDAfter, ownerID)
	}

	var agentRuntimeID, taskRuntimeID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&agentRuntimeID); err != nil {
		t.Fatalf("read agent runtime_id: %v", err)
	}
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent_task_queue WHERE id = $1`, taskID).Scan(&taskRuntimeID); err != nil {
		t.Fatalf("read task runtime_id: %v", err)
	}
	if agentRuntimeID != runtimeID {
		t.Fatalf("agent binding changed: got %s want %s", agentRuntimeID, runtimeID)
	}
	if taskRuntimeID != runtimeID {
		t.Fatalf("task binding changed: got %s want %s", taskRuntimeID, runtimeID)
	}
}
