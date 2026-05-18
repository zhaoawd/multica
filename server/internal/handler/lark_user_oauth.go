package handler

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Lark user OAuth — see LARK_INTEGRATION_DESIGN.md §6.7 / Phase 2.
//
// One link per multica user, mapping user_id ↔ lark open_id. The link is
// required for card-action callbacks (P2.2) so we can answer "who clicked
// this button?" without trusting the request body.
//
// Endpoints:
//   GET    /api/users/me/lark/link            — status (protected)
//   POST   /api/users/me/lark/link            — start OAuth, returns authorize URL (protected)
//   DELETE /api/users/me/lark/link            — unlink (protected)
//   GET    /api/lark/oauth/callback           — Lark redirect target (public — state HMAC carries identity)
//
// The callback is public on purpose: Lark redirects the browser to it and
// we can't require an auth header on a top-level navigation. The signed
// state token (HMAC keyed on LARK_VERIFICATION_TOKEN) carries the
// multica user_id; the callback trusts only that signature, not any
// header or cookie.

// LarkUserLinkResponse is the JSON shape for GET /api/users/me/lark/link.
// When Linked is false, OpenID / LinkedAt are zero values.
//
// Configured mirrors the workspace binding response so the front-end can
// hide the Connect affordance entirely on deployments that haven't set
// LARK_APP_ID etc.
type LarkUserLinkResponse struct {
	Linked     bool   `json:"linked"`
	Configured bool   `json:"configured"`
	OpenID     string `json:"open_id,omitempty"`
	LinkedAt   string `json:"linked_at,omitempty"`
}

// StartLarkUserLinkResponse is the JSON shape for POST /api/users/me/lark/link.
// The frontend redirects the browser to URL (top-level navigation, not
// popup) — Lark's flow requires the user to be on a Lark-controlled page
// to approve.
type StartLarkUserLinkResponse struct {
	URL string `json:"url"`
}

// larkUserStateSecret returns the HMAC key used to sign callback state.
// We reuse LARK_VERIFICATION_TOKEN so operators don't have to configure
// yet another secret — same pattern as github.go reusing the webhook
// secret for its state HMAC.
func larkUserStateSecret() string {
	return strings.TrimSpace(os.Getenv("LARK_VERIFICATION_TOKEN"))
}

// signLarkUserLinkState produces an opaque token binding the OAuth
// callback to (a) the multica user that initiated the flow and (b) the
// in-app path to bounce the browser back to once the link is recorded.
//
// Format: "<userID>.<returnHex>.<nonce>.<sigHex>". The return path is
// hex-encoded so it cannot inject the "." separator; an empty returnPath
// hex-encodes to "" which is fine — verify accepts it and the callback
// falls back to the default landing.
//
// The nonce is per-call so the same user initiating twice doesn't produce
// a re-playable token.
func signLarkUserLinkState(userID, returnPath string) (string, error) {
	secret := larkUserStateSecret()
	if secret == "" {
		return "", errors.New("lark integration is not configured")
	}
	nonceBytes := make([]byte, 12)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	nonce := hex.EncodeToString(nonceBytes)
	returnHex := hex.EncodeToString([]byte(returnPath))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(userID))
	mac.Write([]byte("."))
	mac.Write([]byte(returnHex))
	mac.Write([]byte("."))
	mac.Write([]byte(nonce))
	sig := hex.EncodeToString(mac.Sum(nil))
	return userID + "." + returnHex + "." + nonce + "." + sig, nil
}

// verifyLarkUserLinkState returns (userID, returnPath, ok). returnPath is
// the empty string when the token didn't carry one.
func verifyLarkUserLinkState(token string) (string, string, bool) {
	secret := larkUserStateSecret()
	if secret == "" {
		return "", "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return "", "", false
	}
	userID, returnHex, nonce, sig := parts[0], parts[1], parts[2], parts[3]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(userID))
	mac.Write([]byte("."))
	mac.Write([]byte(returnHex))
	mac.Write([]byte("."))
	mac.Write([]byte(nonce))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return "", "", false
	}
	returnBytes, err := hex.DecodeString(returnHex)
	if err != nil {
		return "", "", false
	}
	return userID, string(returnBytes), true
}

// sanitizeReturnPath defends against the callback being used as an open
// redirect. We require a relative path that starts with a single "/" and
// has no scheme/host. Anything else is rejected and the default landing
// is used. The cap keeps a malicious caller from blowing up state size.
func sanitizeReturnPath(p string) string {
	const maxLen = 512
	p = strings.TrimSpace(p)
	if p == "" || len(p) > maxLen {
		return ""
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return ""
	}
	// No scheme-relative or absolute URLs allowed.
	if strings.Contains(p, "://") {
		return ""
	}
	return p
}

// larkUserRedirectURI is the URL Lark redirects to after the user approves
// the OAuth consent screen. Must match exactly what the operator
// registered in the Lark app's "Redirect URL" allowlist.
//
// We derive it from FRONTEND_ORIGIN because in both dev and self-host the
// backend is behind the frontend (Next.js proxies /api/*), and that proxy
// hop is the only path that gives us a single public URL the operator can
// paste into the Lark app config. If a future deployment exposes the
// backend directly, an explicit LARK_OAUTH_REDIRECT_URI env override can
// be added then — premature now.
func larkUserRedirectURI() string {
	frontend := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	return strings.TrimRight(frontend, "/") + "/api/lark/oauth/callback"
}

// GetMyLarkUserLink returns the current link state for the calling user.
func (h *Handler) GetMyLarkUserLink(w http.ResponseWriter, r *http.Request) {
	userID := requestUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	configured := service.LarkConfigFromEnv().Configured()
	link, err := h.Queries.GetLarkUserLink(r.Context(), userUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, LarkUserLinkResponse{Linked: false, Configured: configured})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load lark link")
		return
	}
	writeJSON(w, http.StatusOK, LarkUserLinkResponse{
		Linked:     true,
		Configured: configured,
		OpenID:     link.LarkOpenID,
		LinkedAt:   timestampToString(link.LinkedAt),
	})
}

// StartLarkUserLinkRequest is the (optional) JSON body for POST. Frontend
// passes the in-app path it wants the user bounced to after the link is
// recorded — typically the same settings page that hosted the Connect
// button. Empty / invalid paths fall back to the default landing.
type StartLarkUserLinkRequest struct {
	ReturnTo string `json:"return_to,omitempty"`
}

// StartLarkUserLink kicks off the OAuth flow. The frontend opens the
// returned URL as a top-level navigation; Lark hosts the consent screen,
// then redirects to /api/lark/oauth/callback with code+state.
func (h *Handler) StartLarkUserLink(w http.ResponseWriter, r *http.Request) {
	userID := requestUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	if _, ok := parseUUIDOrBadRequest(w, userID, "user id"); !ok {
		return
	}
	cfg := service.LarkConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusFailedDependency, "lark integration is not configured on this server")
		return
	}
	// Body is optional — a missing/empty body is fine.
	var req StartLarkUserLinkRequest
	if r.Body != nil && r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	returnPath := sanitizeReturnPath(req.ReturnTo)
	state, err := signLarkUserLinkState(userID, returnPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to sign state")
		return
	}
	client := service.NewLarkClient(cfg)
	authURL := client.BuildAuthorizeURL(larkUserRedirectURI(), state)
	writeJSON(w, http.StatusOK, StartLarkUserLinkResponse{URL: authURL})
}

// LarkUserOAuthCallback handles Lark's redirect after the user approves
// or denies the consent screen. We trust only the HMAC-signed state —
// everything else (code, cookies, headers) is treated as untrusted input.
//
// On success the user is bounced to /settings?lark_linked=1; on every
// failure path we redirect with ?lark_error=<reason> rather than
// rendering a JSON error, because this is a top-level browser
// navigation and the user has nowhere else to land.
func (h *Handler) LarkUserOAuthCallback(w http.ResponseWriter, r *http.Request) {
	// landingFor builds the bounce-back URL. The returnPath from state has
	// already been sanitized at sign time; we still re-run it here so
	// changes to sanitizeReturnPath are enforced retroactively.
	frontend := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	frontend = strings.TrimRight(frontend, "/")
	landingFor := func(returnPath, query string) string {
		path := sanitizeReturnPath(returnPath)
		if path == "" {
			path = "/settings"
		}
		return frontend + path + "?" + query
	}
	fail := func(returnPath, reason string) {
		http.Redirect(w, r, landingFor(returnPath, "lark_error="+url.QueryEscape(reason)), http.StatusFound)
	}

	q := r.URL.Query()
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		fail("", "missing_params")
		return
	}
	userID, returnPath, ok := verifyLarkUserLinkState(state)
	if !ok {
		fail("", "invalid_state")
		return
	}
	userUUID, err := parseStrictUUID(userID)
	if err != nil {
		fail(returnPath, "bad_user")
		return
	}
	cfg := service.LarkConfigFromEnv()
	if !cfg.Configured() {
		fail(returnPath, "not_configured")
		return
	}

	client := service.NewLarkClient(cfg)
	result, err := client.ExchangeOIDCCode(r.Context(), code)
	if err != nil {
		fail(returnPath, "exchange_failed")
		return
	}

	// Encrypt the refresh token before persistence. EncryptBotToken
	// short-circuits to nil for empty input — Lark sometimes omits the
	// refresh_token field (depends on app config / scopes) and we still
	// want to record the link with just the open_id in that case.
	refreshEnc, err := service.EncryptBotToken(cfg.EncryptKey, result.RefreshToken)
	if err != nil {
		fail(returnPath, "encrypt_failed")
		return
	}
	if refreshEnc == nil {
		refreshEnc = []byte{}
	}

	// Upsert: re-linking the same multica user overwrites the previous
	// open_id (operator may have moved to a different Lark identity). The
	// UNIQUE constraint on lark_open_id can still trip if the new open_id
	// is already claimed by another multica user — surface that as a
	// distinct error so the user knows the cause.
	if _, err := h.Queries.UpsertLarkUserLink(r.Context(), db.UpsertLarkUserLinkParams{
		UserID:          userUUID,
		LarkOpenID:      result.OpenID,
		RefreshTokenEnc: refreshEnc,
	}); err != nil {
		if isUniqueViolation(err) {
			fail(returnPath, "already_linked_to_other_user")
			return
		}
		fail(returnPath, "persist_failed")
		return
	}
	http.Redirect(w, r, landingFor(returnPath, "lark_linked=1"), http.StatusFound)
}

// DeleteMyLarkUserLink unlinks the calling user. Idempotent: 204 even
// when no row exists, matching DeleteLarkBinding.
func (h *Handler) DeleteMyLarkUserLink(w http.ResponseWriter, r *http.Request) {
	userID := requestUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	if err := h.Queries.DeleteLarkUserLink(r.Context(), userUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete lark link")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
