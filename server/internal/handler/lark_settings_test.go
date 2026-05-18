package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// withLarkEnv flips on the four LARK_* env vars so service.LarkConfigFromEnv
// reports Configured()=true. The values are placeholders — the handler only
// gates on presence, not value validity, for the binding CRUD endpoints.
func withLarkEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LARK_APP_ID", "cli_test_id")
	t.Setenv("LARK_APP_SECRET", "cli_test_secret")
	t.Setenv("LARK_VERIFICATION_TOKEN", "verify_test")
	t.Setenv("LARK_ENCRYPT_KEY", "encrypt_test_key_32_bytes_padding!!")
}

func TestLarkBinding_GetReturnsBoundFalseWhenAbsent(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	cleanupLarkBinding(t)
	withLarkEnv(t)

	req := newRequest(http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/lark/binding", nil)
	req = withURLParam(req, "id", testWorkspaceID)
	rr := httptest.NewRecorder()
	testHandler.GetLarkBinding(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp LarkBindingResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Bound {
		t.Fatalf("expected bound=false")
	}
	if !resp.Configured {
		t.Fatalf("expected configured=true with env set")
	}
	if len(resp.SupportedEvents) == 0 {
		t.Fatalf("expected supported_events to be populated")
	}
}

func TestLarkBinding_UpsertRequiresConfigured(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	// Deliberately do NOT set env — the handler should refuse with 424.
	t.Setenv("LARK_APP_ID", "")
	t.Setenv("LARK_APP_SECRET", "")
	t.Setenv("LARK_VERIFICATION_TOKEN", "")
	t.Setenv("LARK_ENCRYPT_KEY", "")

	body := map[string]any{
		"chat_id":        "oc_xxx",
		"enabled_events": []string{protocol.EventIssueCreated},
	}
	req := newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/lark/binding", body)
	req = withURLParam(req, "id", testWorkspaceID)
	rr := httptest.NewRecorder()
	testHandler.UpsertLarkBinding(rr, req)
	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("expected 424, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestLarkBinding_UpsertRoundtripAndFilter(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	cleanupLarkBinding(t)
	withLarkEnv(t)

	body := map[string]any{
		"chat_id":        "oc_chat_id_test",
		"enabled_events": []string{protocol.EventIssueCreated, "not-a-real-event", protocol.EventIssueCreated},
	}
	req := newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/lark/binding", body)
	req = withURLParam(req, "id", testWorkspaceID)
	rr := httptest.NewRecorder()
	testHandler.UpsertLarkBinding(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp LarkBindingResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Bound {
		t.Fatalf("expected bound=true")
	}
	if resp.ChatID != "oc_chat_id_test" {
		t.Fatalf("chat_id mismatch: %q", resp.ChatID)
	}
	if len(resp.EnabledEvents) != 1 || resp.EnabledEvents[0] != protocol.EventIssueCreated {
		t.Fatalf("unknown + duplicate events not filtered: %v", resp.EnabledEvents)
	}

	// Round-trip via GET to confirm persistence.
	req2 := newRequest(http.MethodGet, "/api/workspaces/"+testWorkspaceID+"/lark/binding", nil)
	req2 = withURLParam(req2, "id", testWorkspaceID)
	rr2 := httptest.NewRecorder()
	testHandler.GetLarkBinding(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rr2.Code)
	}
	var read LarkBindingResponse
	if err := json.NewDecoder(rr2.Body).Decode(&read); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !read.Bound || read.ChatID != "oc_chat_id_test" {
		t.Fatalf("readback mismatch: %+v", read)
	}

	cleanupLarkBinding(t)
}

func TestLarkBinding_PatchUpdatesEventsOnly(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	cleanupLarkBinding(t)
	withLarkEnv(t)

	// Seed.
	seed := map[string]any{
		"chat_id":        "oc_seed",
		"enabled_events": []string{protocol.EventIssueCreated},
	}
	req := newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/lark/binding", seed)
	req = withURLParam(req, "id", testWorkspaceID)
	rr := httptest.NewRecorder()
	testHandler.UpsertLarkBinding(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed failed: %d %s", rr.Code, rr.Body.String())
	}

	// PATCH only events; chat_id should survive.
	patch := map[string]any{
		"enabled_events": []string{protocol.EventTaskCompleted, protocol.EventTaskFailed},
	}
	req2 := newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID+"/lark/binding", patch)
	req2 = withURLParam(req2, "id", testWorkspaceID)
	rr2 := httptest.NewRecorder()
	testHandler.PatchLarkBinding(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("patch status = %d body = %s", rr2.Code, rr2.Body.String())
	}
	var resp LarkBindingResponse
	if err := json.NewDecoder(rr2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ChatID != "oc_seed" {
		t.Fatalf("PATCH should not touch chat_id, got %q", resp.ChatID)
	}
	if len(resp.EnabledEvents) != 2 {
		t.Fatalf("expected 2 events, got %v", resp.EnabledEvents)
	}

	cleanupLarkBinding(t)
}

func TestLarkBinding_DeleteIsIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	cleanupLarkBinding(t)
	withLarkEnv(t)

	for i := 0; i < 2; i++ {
		req := newRequest(http.MethodDelete, "/api/workspaces/"+testWorkspaceID+"/lark/binding", nil)
		req = withURLParam(req, "id", testWorkspaceID)
		rr := httptest.NewRecorder()
		testHandler.DeleteLarkBinding(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("iteration %d: status = %d body = %s", i, rr.Code, rr.Body.String())
		}
	}
}

func cleanupLarkBinding(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM lark_workspace_binding WHERE workspace_id::text = $1`, testWorkspaceID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
