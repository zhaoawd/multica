package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// withLarkWebhookEnv flips on the env vars the webhook handler reads.
// Kept separate from withLarkEnv (settings tests) because the values
// participate in signature computation in this file.
func withLarkWebhookEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LARK_APP_ID", "cli_test_id")
	t.Setenv("LARK_APP_SECRET", "cli_test_secret")
	t.Setenv("LARK_VERIFICATION_TOKEN", "verify_test_token")
	t.Setenv("LARK_ENCRYPT_KEY", "encrypt_test_key")
}

// signLarkBody mirrors the production header-based signature scheme. We
// deliberately keep the test impl alongside the production one — if the
// scheme changes both move together and we notice.
func signLarkBody(ts, nonce, encryptKey string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(ts))
	h.Write([]byte(nonce))
	h.Write([]byte(encryptKey))
	h.Write(body)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestLarkWebhook_URLVerificationChallengeEchoesBackViaTokenFallback(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}

	body, _ := json.Marshal(map[string]string{
		"type":      "url_verification",
		"token":     "verify_test_token",
		"challenge": "challenge_xyz",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["challenge"] != "challenge_xyz" {
		t.Fatalf("expected challenge echoed, got %v", out)
	}
}

func TestLarkWebhook_SignedRequestPasses(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}

	body, _ := json.Marshal(map[string]any{
		"type":      "url_verification",
		"token":     "irrelevant_under_signature",
		"challenge": "abc",
	})
	const ts = "1715900000"
	const nonce = "n1"
	sig := signLarkBody(ts, nonce, "encrypt_test_key", body)

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	req.Header.Set("X-Lark-Request-Timestamp", ts)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
	req.Header.Set("X-Lark-Signature", sig)
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
}

func TestLarkWebhook_TamperedSignatureFails(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}

	body, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": "abc"})
	sig := signLarkBody("1715900000", "n", "encrypt_test_key", body)
	tampered := sig[:len(sig)-1] + "A"

	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	req.Header.Set("X-Lark-Request-Timestamp", "1715900000")
	req.Header.Set("X-Lark-Request-Nonce", "n")
	req.Header.Set("X-Lark-Signature", tampered)
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered sig, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLarkWebhook_WrongTokenFallbackFails(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}
	body, _ := json.Marshal(map[string]string{
		"type":      "url_verification",
		"token":     "not_the_real_token",
		"challenge": "abc",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLarkWebhook_NoSecretsRefusesEverything(t *testing.T) {
	t.Setenv("LARK_ENCRYPT_KEY", "")
	t.Setenv("LARK_VERIFICATION_TOKEN", "")
	h := &Handler{}

	body, _ := json.Marshal(map[string]string{"type": "url_verification", "challenge": "abc"})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when env unset, got %d", rr.Code)
	}
}

func TestLarkWebhook_UnknownActionTagAcked(t *testing.T) {
	withLarkWebhookEnv(t)
	h := &Handler{}
	body, _ := json.Marshal(map[string]any{
		"token":   "verify_test_token",
		"open_id": "ou_x",
		"action": map[string]any{
			"tag":   "select_static",
			"value": map[string]any{"verb": "anything"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected silent 200 ack, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "{}") {
		t.Fatalf("expected empty body, got %s", rr.Body.String())
	}
}

func TestLarkWebhook_UnlinkedClickerGetsLinkPrompt(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)

	body, _ := json.Marshal(map[string]any{
		"token":   "verify_test_token",
		"open_id": "ou_unknown_user",
		"action": map[string]any{
			"tag":   "button",
			"value": map[string]any{"verb": "claim", "issue_id": "00000000-0000-0000-0000-000000000000"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	// Toast type=warning + "Link your Lark account" prompt is the contract.
	bodyStr := rr.Body.String()
	if !strings.Contains(bodyStr, `"type":"warning"`) ||
		!strings.Contains(bodyStr, "Link your Lark account") {
		t.Fatalf("expected link-prompt toast, got %s", bodyStr)
	}
}

func TestLarkWebhook_ClaimVerb_AssignsIssueToClicker(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)

	// Seed: link this test's multica user to a Lark open_id, and create
	// an unassigned issue in the test workspace.
	const openID = "ou_claim_happy_path"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'Claim me', '', 'todo', 'medium', 'member', $2, 9001)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	body, _ := json.Marshal(map[string]any{
		"token":   "verify_test_token",
		"open_id": openID,
		"action": map[string]any{
			"tag":   "button",
			"value": map[string]any{"verb": "claim", "issue_id": issueID},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"type":"success"`) {
		t.Fatalf("expected success toast, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Verify the DB now has the issue assigned to the test user.
	var assigneeType pgtype.Text
	var assigneeID pgtype.UUID
	if err := testPool.QueryRow(context.Background(),
		`SELECT assignee_type, assignee_id FROM issue WHERE id = $1`, issueID,
	).Scan(&assigneeType, &assigneeID); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !assigneeType.Valid || assigneeType.String != "member" {
		t.Fatalf("assignee_type = %+v, want member", assigneeType)
	}
	if !assigneeID.Valid {
		t.Fatalf("assignee_id not set")
	}
}

func TestLarkWebhook_MarkDoneVerb_FlipsStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)

	const openID = "ou_mark_done_happy_path"
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	var issueID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO issue (workspace_id, title, description, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'Finish me', '', 'in_progress', 'medium', 'member', $2, 9002)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	body, _ := json.Marshal(map[string]any{
		"token":   "verify_test_token",
		"open_id": openID,
		"action": map[string]any{
			"tag":   "button",
			"value": map[string]any{"verb": "mark_done", "issue_id": issueID},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"type":"success"`) {
		t.Fatalf("expected success, got %d body=%s", rr.Code, rr.Body.String())
	}

	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
}
