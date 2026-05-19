// Package service — Lark (Feishu) integration low-level primitives.
//
// This file holds:
//   - LarkConfig: env-derived credentials.
//   - LarkClient: a tiny HTTP client that talks to Lark's Open API (tenant
//     access token + send-message). We deliberately do not pull in the heavy
//     larksuite/oapi-sdk-go for Phase 1 — the only endpoints we touch
//     (auth/v3/tenant_access_token/internal and im/v1/messages) are stable
//     and trivial. When P3 (docs) and later phases need richer Lark APIs we
//     can swap to the SDK without touching call sites that go through this
//     interface.
//   - encryptBotToken / decryptBotToken: AES-GCM round-trip with
//     LARK_ENCRYPT_KEY. P1 doesn't store per-workspace bot tokens (we use the
//     app-level tenant token), but the schema field exists for later phases
//     and the encrypt helpers ship now so the surface is ready.
package service

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	larkAPIBase             = "https://open.feishu.cn/open-apis"
	larkTokenPath           = "/auth/v3/tenant_access_token/internal"
	larkSendMessagePath     = "/im/v1/messages?receive_id_type=chat_id"
	larkReplyMessagePath    = "/im/v1/messages/%s/reply"
	larkListMessagesPath    = "/im/v1/messages"
	larkAuthorizeURL        = "https://accounts.feishu.cn/open-apis/authen/v1/authorize"
	larkOIDCAccessTokenPath = "/authen/v1/oidc/access_token"
	larkTokenSafetyMargin   = 60 * time.Second // refresh slightly before expiry
	larkDefaultHTTPTimeout  = 10 * time.Second

	// LarkMaxThreadFetchMessages caps how many messages from a Lark thread
	// the @bot 创建任务 verb pulls into the issue description. The list API
	// pages at 50; we stop short of one page so even a chatty thread stays
	// within a single API call. Older messages are dropped — issue body is
	// human-readable text, not a forensic transcript.
	LarkMaxThreadFetchMessages = 30
)

// LarkConfig captures the four env vars that gate the Lark integration.
// All four must be set for the integration to be considered configured —
// otherwise the integration is silently disabled (notifier no-ops, settings
// UI shows "not configured", listeners do not fire).
type LarkConfig struct {
	AppID              string
	AppSecret          string
	VerificationToken  string
	EncryptKey         string
}

// LarkConfigFromEnv reads the four LARK_* env vars.
func LarkConfigFromEnv() LarkConfig {
	return LarkConfig{
		AppID:             strings.TrimSpace(os.Getenv("LARK_APP_ID")),
		AppSecret:         strings.TrimSpace(os.Getenv("LARK_APP_SECRET")),
		VerificationToken: strings.TrimSpace(os.Getenv("LARK_VERIFICATION_TOKEN")),
		EncryptKey:        strings.TrimSpace(os.Getenv("LARK_ENCRYPT_KEY")),
	}
}

// Configured returns true only when every credential is present.
// A partially-configured deployment is treated as disabled rather than
// half-working — the UI surfaces this so the operator knows to finish.
func (c LarkConfig) Configured() bool {
	return c.AppID != "" && c.AppSecret != "" && c.VerificationToken != "" && c.EncryptKey != ""
}

// LarkClient is a minimal Lark Open API client.
// One instance is shared across the process; the embedded tokenCache
// memoizes the app-level tenant_access_token until close to expiry.
//
// apiBase is overridable only via the test-only setter (see SetAPIBaseForTest);
// production code always uses the const larkAPIBase. We carry it as a field
// rather than mutating package-level state so parallel tests don't race.
type LarkClient struct {
	cfg        LarkConfig
	httpClient *http.Client
	apiBase    string

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewLarkClient constructs a client. Callers should check cfg.Configured()
// before invoking any method; we still return a usable struct when
// disabled so wiring code doesn't need nil-checks on every call site.
func NewLarkClient(cfg LarkConfig) *LarkClient {
	return &LarkClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: larkDefaultHTTPTimeout},
		apiBase:    larkAPIBase,
	}
}

// SetAPIBaseForTest substitutes the Lark API base URL — only intended
// for tests in other packages that need to point a real *LarkClient at
// a httptest.Server. Calling this from production code is a bug; the
// production base is the const larkAPIBase set in NewLarkClient.
//
// We expose this rather than making apiBase exported because production
// code has no reason to mutate it and a public field invites accidents.
func SetAPIBaseForTest(c *LarkClient, base string) {
	if c == nil {
		return
	}
	c.apiBase = base
}

// tenantAccessToken returns a valid app-level tenant_access_token,
// refreshing on first call and whenever the cached token is about to
// expire. Mutex-serialised so concurrent issue-created events don't
// dogpile the auth endpoint.
func (c *LarkClient) tenantAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Add(larkTokenSafetyMargin).Before(c.tokenExp) {
		return c.token, nil
	}
	body, err := json.Marshal(map[string]string{
		"app_id":     c.cfg.AppID,
		"app_secret": c.cfg.AppSecret,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+larkTokenPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return "", fmt.Errorf("lark token: code=%d msg=%s", out.Code, out.Msg)
	}
	c.token = out.TenantAccessToken
	// Expire is in seconds; default Lark TTL is ~2h. Cache slightly less to
	// avoid sending a stale token mid-flight.
	if out.Expire <= 0 {
		out.Expire = 5400
	}
	c.tokenExp = time.Now().Add(time.Duration(out.Expire) * time.Second)
	return c.token, nil
}

// SendInteractiveCard posts an interactive card to a chat by chat_id.
// The card argument is the JSON-serialisable card structure per Lark's
// "message-card" spec. Returns an error if the API call fails.
func (c *LarkClient) SendInteractiveCard(ctx context.Context, chatID string, card any) error {
	if !c.cfg.Configured() {
		return errors.New("lark not configured")
	}
	if chatID == "" {
		return errors.New("chat_id required")
	}
	cardBytes, err := json.Marshal(card)
	if err != nil {
		return err
	}
	payload := map[string]string{
		"receive_id": chatID,
		"msg_type":   "interactive",
		"content":    string(cardBytes),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+larkSendMessagePath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("lark send: bad response (%d): %s", resp.StatusCode, string(raw))
	}
	if out.Code != 0 {
		return fmt.Errorf("lark send: code=%d msg=%s", out.Code, out.Msg)
	}
	return nil
}

// ── Thread / reply (P4) ─────────────────────────────────────────────────

// LarkThreadMessage is a single message returned by Lark's /im/v1/messages
// list endpoint, projected into the fields the @bot 创建任务 flow needs.
//
// Text is the message body extracted from Lark's wire format (Lark
// embeds text messages as `{"text":"..."}` in `content`). For non-text
// messages (image, file, sticker, ...) Text is the empty string —
// callers either skip the message or render a placeholder.
type LarkThreadMessage struct {
	MessageID    string
	SenderOpenID string
	CreatedAt    time.Time
	Text         string
}

// ReplyToMessage posts a text reply to a previously-sent Lark message.
// Used by P4's @bot 创建任务 to confirm "已创建 multica-NN" back into
// the thread the user @-mentioned the bot in. Lark's reply endpoint
// auto-threads the response when `reply_in_thread=true` is passed, which
// keeps the conversation grouped in the Lark client UI.
//
// `text` is plain text. Lark renders it inside a system "rich text"
// payload, so we marshal it through their expected `{"text":"..."}`
// envelope. No card / Markdown — keep the @bot reply boring and clear.
func (c *LarkClient) ReplyToMessage(ctx context.Context, messageID, text string) error {
	if !c.cfg.Configured() {
		return errors.New("lark not configured")
	}
	if messageID == "" {
		return errors.New("message_id required")
	}
	contentBytes, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	payload := map[string]any{
		"content":         string(contentBytes),
		"msg_type":        "text",
		"reply_in_thread": true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf(c.apiBase+larkReplyMessagePath, messageID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("lark reply: bad response (%d): %s", resp.StatusCode, string(raw))
	}
	if out.Code != 0 {
		return fmt.Errorf("lark reply: code=%d msg=%s", out.Code, out.Msg)
	}
	return nil
}

// ListThreadMessages returns up to `limit` messages from a Lark thread,
// ordered oldest → newest (the same order the user sees them).
//
// `threadID` is the `thread_id` field from Lark's event subscription
// payload, which Lark also accepts as the `container_id` query parameter.
// The list endpoint pages — for P4 we only fetch the first page and cap
// at LarkMaxThreadFetchMessages, since the issue body is built for human
// readability, not transcript fidelity.
func (c *LarkClient) ListThreadMessages(ctx context.Context, threadID string, limit int) ([]LarkThreadMessage, error) {
	if !c.cfg.Configured() {
		return nil, errors.New("lark not configured")
	}
	if threadID == "" {
		return nil, errors.New("thread_id required")
	}
	if limit <= 0 || limit > LarkMaxThreadFetchMessages {
		limit = LarkMaxThreadFetchMessages
	}

	q := url.Values{}
	q.Set("container_id_type", "thread")
	q.Set("container_id", threadID)
	q.Set("sort_type", "ByCreateTimeAsc")
	q.Set("page_size", fmt.Sprintf("%d", limit))

	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := c.apiBase + larkListMessagesPath + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Items []struct {
				MessageID  string `json:"message_id"`
				CreateTime string `json:"create_time"` // Lark sends milliseconds as a string
				MsgType    string `json:"msg_type"`
				Body       struct {
					Content string `json:"content"`
				} `json:"body"`
				Sender struct {
					ID     string `json:"id"`
					IDType string `json:"id_type"`
				} `json:"sender"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lark thread list: bad response (%d): %s", resp.StatusCode, string(raw))
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("lark thread list: code=%d msg=%s", out.Code, out.Msg)
	}

	msgs := make([]LarkThreadMessage, 0, len(out.Data.Items))
	for _, item := range out.Data.Items {
		// Lark text messages embed the visible body as JSON
		// `{"text":"..."}` in body.content. Other msg_types (post, image,
		// file, sticker, ...) come through here too but with no
		// human-readable text we can pull cleanly — skip them.
		text := ""
		if item.MsgType == "text" && item.Body.Content != "" {
			var inner struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(item.Body.Content), &inner); err == nil {
				text = inner.Text
			}
		}
		msgs = append(msgs, LarkThreadMessage{
			MessageID:    item.MessageID,
			SenderOpenID: item.Sender.ID,
			CreatedAt:    parseLarkCreateTime(item.CreateTime),
			Text:         text,
		})
	}
	return msgs, nil
}

// parseLarkCreateTime decodes Lark's create_time string (epoch
// milliseconds, sent as a quoted JSON string) into time.Time. Returns
// the zero value on parse failure so callers can fall back gracefully
// — the timestamp is informational in the issue description, not a
// foreign key.
func parseLarkCreateTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// ── User OAuth (P2) ─────────────────────────────────────────────────────────

// LarkOIDCResult is the subset of fields we use from the OIDC token
// endpoint. Lark returns more (avatar, en_name, mobile, employee_id, ...) but
// P2 only needs the identity (open_id) and a refresh token to mint future
// user-scoped access tokens. The optional Name / Email are stored only to
// improve the UI ("connected as <name>") — they are not persisted.
type LarkOIDCResult struct {
	OpenID       string
	UnionID      string
	AccessToken  string
	RefreshToken string
	Name         string
	Email        string
	AvatarURL    string
}

// BuildAuthorizeURL returns the Feishu OAuth authorize URL the browser must
// be redirected to. state is opaque to Lark; the caller HMAC-signs it and
// re-verifies on the callback to prevent CSRF.
//
// redirect_uri must match what the Lark app's "Security settings" allows.
// We don't validate that here — Lark rejects mismatches at the authorize step
// with a clear error message, which is the right place for the operator to
// see it.
func (c *LarkClient) BuildAuthorizeURL(redirectURI, state string) string {
	q := url.Values{}
	q.Set("app_id", c.cfg.AppID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	return larkAuthorizeURL + "?" + q.Encode()
}

// ExchangeOIDCCode swaps the OAuth ?code returned on the callback for the
// user's open_id, a short-lived user access_token and a longer-lived
// refresh_token. Uses the app-level tenant_access_token as the Bearer for
// the exchange call, per Lark's v1 OIDC flow.
//
// Errors are returned verbatim with the Lark code/msg so an operator
// reading server logs can tell "wrong app secret" from "expired code" from
// "redirect_uri mismatch" without needing to dig into the SDK.
func (c *LarkClient) ExchangeOIDCCode(ctx context.Context, code string) (LarkOIDCResult, error) {
	if !c.cfg.Configured() {
		return LarkOIDCResult{}, errors.New("lark not configured")
	}
	if strings.TrimSpace(code) == "" {
		return LarkOIDCResult{}, errors.New("code required")
	}
	appToken, err := c.tenantAccessToken(ctx)
	if err != nil {
		return LarkOIDCResult{}, err
	}
	body, err := json.Marshal(map[string]string{
		"grant_type": "authorization_code",
		"code":       code,
	})
	if err != nil {
		return LarkOIDCResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+larkOIDCAccessTokenPath, bytes.NewReader(body))
	if err != nil {
		return LarkOIDCResult{}, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+appToken)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return LarkOIDCResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			OpenID       string `json:"open_id"`
			UnionID      string `json:"union_id"`
			Name         string `json:"name"`
			Email        string `json:"email"`
			AvatarURL    string `json:"avatar_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return LarkOIDCResult{}, fmt.Errorf("lark oidc: bad response (%d): %s", resp.StatusCode, string(raw))
	}
	if out.Code != 0 || out.Data.OpenID == "" {
		return LarkOIDCResult{}, fmt.Errorf("lark oidc: code=%d msg=%s", out.Code, out.Msg)
	}
	return LarkOIDCResult{
		OpenID:       out.Data.OpenID,
		UnionID:      out.Data.UnionID,
		AccessToken:  out.Data.AccessToken,
		RefreshToken: out.Data.RefreshToken,
		Name:         out.Data.Name,
		Email:        out.Data.Email,
		AvatarURL:    out.Data.AvatarURL,
	}, nil
}

// ── Bot / refresh-token encryption (shared between P1 bot_token_enc and P2 user refresh_token_enc) ─────

// EncryptBotToken AES-GCM-encrypts a bot token with LARK_ENCRYPT_KEY.
// The key may be supplied as raw text, hex, or base64; we hash it to 32
// bytes so any reasonably-strong operator-chosen value works.
// Output layout: nonce || ciphertext || tag — suitable for direct BYTEA storage.
func EncryptBotToken(key, plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	gcm, err := newLarkAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// DecryptBotToken reverses EncryptBotToken. An empty input returns "".
func DecryptBotToken(key string, blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	gcm, err := newLarkAEAD(key)
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return "", errors.New("lark: token blob too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func newLarkAEAD(key string) (cipher.AEAD, error) {
	if key == "" {
		return nil, errors.New("LARK_ENCRYPT_KEY is empty")
	}
	// Accept hex, base64, or raw — sha256 normalises whatever the operator
	// chose into a fixed 32-byte AES-256 key. We don't enforce a specific
	// format so the env-pasting story stays operator-friendly.
	raw := []byte(key)
	if dec, err := hex.DecodeString(key); err == nil && len(dec) >= 16 {
		raw = dec
	} else if dec, err := base64.StdEncoding.DecodeString(key); err == nil && len(dec) >= 16 {
		raw = dec
	}
	sum := sha256.Sum256(raw)
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
