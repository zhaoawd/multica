package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLarkUserLinkState_SignVerifyRoundtrip(t *testing.T) {
	t.Setenv("LARK_VERIFICATION_TOKEN", "verify_test_secret")

	const userID = "11111111-1111-1111-1111-111111111111"
	const returnPath = "/my-workspace/settings"
	state, err := signLarkUserLinkState(userID, returnPath)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	gotUser, gotPath, ok := verifyLarkUserLinkState(state)
	if !ok {
		t.Fatalf("verify failed for valid state")
	}
	if gotUser != userID {
		t.Fatalf("user_id mismatch: got %q want %q", gotUser, userID)
	}
	if gotPath != returnPath {
		t.Fatalf("return_path mismatch: got %q want %q", gotPath, returnPath)
	}
}

func TestLarkUserLinkState_EmptyReturnPathRoundtrips(t *testing.T) {
	t.Setenv("LARK_VERIFICATION_TOKEN", "verify_test_secret")
	state, err := signLarkUserLinkState("11111111-1111-1111-1111-111111111111", "")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, gotPath, ok := verifyLarkUserLinkState(state)
	if !ok || gotPath != "" {
		t.Fatalf("expected empty return path, got %q ok=%v", gotPath, ok)
	}
}

func TestLarkUserLinkState_TamperedSigFails(t *testing.T) {
	t.Setenv("LARK_VERIFICATION_TOKEN", "verify_test_secret")

	state, err := signLarkUserLinkState("11111111-1111-1111-1111-111111111111", "/x/settings")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// Flip the last hex char of the signature segment.
	parts := strings.Split(state, ".")
	if len(parts) != 4 {
		t.Fatalf("unexpected state shape: %q", state)
	}
	last := parts[3]
	flipped := "0"
	if last[len(last)-1] == '0' {
		flipped = "1"
	}
	parts[3] = last[:len(last)-1] + flipped
	if _, _, ok := verifyLarkUserLinkState(strings.Join(parts, ".")); ok {
		t.Fatalf("tampered signature should not verify")
	}
}

func TestLarkUserLinkState_DifferentSecretFails(t *testing.T) {
	t.Setenv("LARK_VERIFICATION_TOKEN", "first_secret")
	state, err := signLarkUserLinkState("11111111-1111-1111-1111-111111111111", "/x")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	t.Setenv("LARK_VERIFICATION_TOKEN", "second_secret")
	if _, _, ok := verifyLarkUserLinkState(state); ok {
		t.Fatalf("state should not verify under a different secret")
	}
}

func TestLarkUserLinkState_NoSecretRefusesToSign(t *testing.T) {
	t.Setenv("LARK_VERIFICATION_TOKEN", "")
	if _, err := signLarkUserLinkState("user", ""); err == nil {
		t.Fatalf("expected error when LARK_VERIFICATION_TOKEN is unset")
	}
}

func TestSanitizeReturnPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/workspace/settings", "/workspace/settings"},
		{"/settings?tab=profile", "/settings?tab=profile"},
		{"", ""},
		{"settings", ""},                              // missing leading slash
		{"//evil.com/path", ""},                       // protocol-relative
		{"https://evil.com/x", ""},                    // absolute URL
		{"/x://evil.com", ""},                         // smuggled scheme
		{strings.Repeat("/a", 400), ""},               // length cap
	}
	for _, c := range cases {
		if got := sanitizeReturnPath(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLarkUserOAuth_CallbackInvalidStateRedirects(t *testing.T) {
	t.Setenv("LARK_VERIFICATION_TOKEN", "verify_test_secret")
	t.Setenv("FRONTEND_ORIGIN", "http://example.test")

	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/lark/oauth/callback?code=abc&state=not-a-valid-state", nil)
	rr := httptest.NewRecorder()
	h.LarkUserOAuthCallback(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d (want 302)", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/settings") || !strings.Contains(loc, "lark_error=invalid_state") {
		t.Fatalf("unexpected redirect: %s", loc)
	}
}

func TestLarkUserOAuth_CallbackMissingParamsRedirects(t *testing.T) {
	t.Setenv("FRONTEND_ORIGIN", "http://example.test")

	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/api/lark/oauth/callback", nil)
	rr := httptest.NewRecorder()
	h.LarkUserOAuthCallback(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d (want 302)", rr.Code)
	}
	if !strings.Contains(rr.Header().Get("Location"), "lark_error=missing_params") {
		t.Fatalf("unexpected redirect: %s", rr.Header().Get("Location"))
	}
}

func TestLarkUserLink_GetReturnsLinkedFalseWhenAbsent(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	cleanupLarkUserLink(t)
	withLarkEnv(t)

	req := newRequest(http.MethodGet, "/api/users/me/lark/link", nil)
	rr := httptest.NewRecorder()
	testHandler.GetMyLarkUserLink(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp LarkUserLinkResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Linked {
		t.Fatalf("expected linked=false")
	}
	if !resp.Configured {
		t.Fatalf("expected configured=true with env set")
	}
}

func TestLarkUserLink_StartRequiresConfigured(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	t.Setenv("LARK_APP_ID", "")
	t.Setenv("LARK_APP_SECRET", "")
	t.Setenv("LARK_VERIFICATION_TOKEN", "")
	t.Setenv("LARK_ENCRYPT_KEY", "")

	req := newRequest(http.MethodPost, "/api/users/me/lark/link", nil)
	rr := httptest.NewRecorder()
	testHandler.StartLarkUserLink(rr, req)

	if rr.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d (want 424) body = %s", rr.Code, rr.Body.String())
	}
}

func TestLarkUserLink_StartReturnsAuthorizeURL(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkEnv(t)
	t.Setenv("FRONTEND_ORIGIN", "http://example.test")

	req := newRequest(http.MethodPost, "/api/users/me/lark/link", nil)
	rr := httptest.NewRecorder()
	testHandler.StartLarkUserLink(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp StartLarkUserLinkResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.URL, "https://accounts.feishu.cn/open-apis/authen/v1/authorize?") {
		t.Fatalf("unexpected authorize URL: %s", resp.URL)
	}
	if !strings.Contains(resp.URL, "app_id=cli_test_id") {
		t.Fatalf("authorize URL missing app_id: %s", resp.URL)
	}
	if !strings.Contains(resp.URL, "state=") {
		t.Fatalf("authorize URL missing state: %s", resp.URL)
	}
}

func TestLarkUserLink_DeleteIsIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	cleanupLarkUserLink(t)

	for i := 0; i < 2; i++ {
		req := newRequest(http.MethodDelete, "/api/users/me/lark/link", nil)
		rr := httptest.NewRecorder()
		testHandler.DeleteMyLarkUserLink(rr, req)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("iter %d: status = %d body = %s", i, rr.Code, rr.Body.String())
		}
	}
}

func cleanupLarkUserLink(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM lark_user_link WHERE user_id::text = $1`, testUserID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
