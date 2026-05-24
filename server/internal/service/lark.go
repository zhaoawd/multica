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
//
// Attachments enumerates the binary payloads referenced by image / file
// messages. The §14.1.3 thread-media bridge downloads these via
// DownloadMessageResource and attaches them to the new issue. Text-
// only messages have Attachments == nil.
type LarkThreadMessage struct {
	MessageID    string
	SenderOpenID string
	CreatedAt    time.Time
	Text         string
	Attachments  []LarkMessageAttachment
}

// LarkMessageAttachment is the metadata needed to download one binary
// payload attached to a Lark message. ResourceType is the value of
// Lark's `?type=` query parameter on the resources endpoint —
// "image" for image messages, "file" for file/media messages.
//
// Filename is best-effort: image messages don't carry a user-friendly
// name (Lark generates one), file messages do. The downloader
// substitutes a fallback when Filename is empty so the issue
// attachment list still has something to display.
//
// SizeHint comes from the message envelope where Lark provides it
// (file messages). For image messages it's zero and the size is
// only known after the download completes. The downloader uses
// SizeHint > 0 to short-circuit before the HTTP body is fetched.
type LarkMessageAttachment struct {
	FileKey      string
	ResourceType string // "image" | "file"
	Filename     string
	MimeType     string
	SizeHint     int64
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

// SendTextMessage posts a plain-text message to a recipient identified
// by `receiveIDType` (the value of Lark's `receive_id_type` query
// parameter — `chat_id`, `open_id`, `union_id`, `user_id`, or `email`)
// and `receiveID` of the matching kind.
//
// Used by §14.1.2's `/whoami` DM fan-out (receive_id_type=open_id) and
// any future "send by user id" path. For chat_id sends we still prefer
// SendInteractiveCard so the rich card surface stays — this is for
// short, line-oriented text where the recipient hint is a user, not
// a chat.
//
// Errors propagate verbatim; callers decide whether to log-and-drop
// (best-effort) or surface (interactive). Validation: empty inputs
// fail fast so a misconfigured caller can't trigger a Lark 400 with
// useful detail buried under several layers of HTTP plumbing.
func (c *LarkClient) SendTextMessage(ctx context.Context, receiveIDType, receiveID, text string) error {
	if !c.cfg.Configured() {
		return errors.New("lark not configured")
	}
	if receiveIDType == "" {
		return errors.New("receive_id_type required")
	}
	if receiveID == "" {
		return errors.New("receive_id required")
	}
	contentBytes, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	payload := map[string]string{
		"receive_id": receiveID,
		"msg_type":   "text",
		"content":    string(contentBytes),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	endpoint := c.apiBase + "/im/v1/messages?receive_id_type=" + receiveIDType
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
		return fmt.Errorf("lark send text: bad response (%d): %s", resp.StatusCode, string(raw))
	}
	if out.Code != 0 {
		return fmt.Errorf("lark send text: code=%d msg=%s", out.Code, out.Msg)
	}
	return nil
}

// LarkResourceErrKind categorises why a DownloadMessageResource call
// failed, so callers can map to the right §14.1.3 placeholder line
// without grepping the raw error string:
//
//   - PermissionDenied: Lark returned 401/403 OR an im:resource-scope
//     error code. Triggers the "ask admin to grant im:resource" bot
//     reply (throttled via lark_workspace_binding.last_perm_warning_at).
//   - NotFound: Lark returned 404. Renders as
//     "[attachment unavailable: <filename>]" in the issue body.
//   - TooLarge: caller-supplied maxBytes exceeded mid-stream. We do NOT
//     read the rest of the body — the limited reader short-circuits at
//     maxBytes+1. Caller maps this to "oversized" so the user sees a
//     clean placeholder instead of a generic "unavailable".
//   - Other: every other failure (network, malformed response, etc.).
//     Same surface as NotFound — degraded but doesn't escalate to a
//     thread reply.
type LarkResourceErrKind int

const (
	LarkResourceErrNone LarkResourceErrKind = iota
	LarkResourceErrPermissionDenied
	LarkResourceErrNotFound
	LarkResourceErrTooLarge
	LarkResourceErrOther
)

// LarkResourceError wraps the categorised kind with the underlying
// detail. Returned by DownloadMessageResource on every non-2xx
// outcome; callers should classify via .Kind, not the wrapped error.
type LarkResourceError struct {
	Kind LarkResourceErrKind
	Err  error
}

func (e *LarkResourceError) Error() string {
	if e == nil || e.Err == nil {
		return "lark resource error"
	}
	return e.Err.Error()
}

// LarkResourceDownloadCap is the byte ceiling enforced inside the
// client when the caller passes maxBytes<=0. Above this we abort the
// read and return ErrTooLarge so the server doesn't buffer a multi-GB
// blob into memory just because a user attached one to a Lark thread.
// §14.1.3 enforces a tighter 10 MB per-file cap at the media-service
// layer; passing the tighter limit via maxBytes ensures images with
// no envelope size hint cannot burn the full 64 MB before being
// rejected post-download.
const LarkResourceDownloadCap = 64 * 1024 * 1024 // 64 MB

// DownloadMessageResource fetches one binary attached to a Lark
// message. The endpoint is `/im/v1/messages/{message_id}/resources/{file_key}?type=image|file`.
//
// maxBytes bounds the read; pass 0 to use LarkResourceDownloadCap.
// When the resource exceeds the effective limit, the returned error
// is LarkResourceErrTooLarge — callers should map that to "oversized"
// (or "limit_exhausted" if the caller's effective max was a remaining
// budget) rather than generic unavailable.
//
// Returns the body bytes and the server-reported Content-Type. Both
// are useful: the body is what we upload to multica storage, and the
// Content-Type is the authoritative mime for image messages (the
// list endpoint doesn't disclose the subtype).
func (c *LarkClient) DownloadMessageResource(ctx context.Context, messageID, fileKey, resourceType string, maxBytes int64) ([]byte, string, *LarkResourceError) {
	if !c.cfg.Configured() {
		return nil, "", &LarkResourceError{Kind: LarkResourceErrOther, Err: errors.New("lark not configured")}
	}
	if messageID == "" || fileKey == "" {
		return nil, "", &LarkResourceError{Kind: LarkResourceErrOther, Err: errors.New("message_id and file_key required")}
	}
	if resourceType == "" {
		resourceType = "image"
	}

	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return nil, "", &LarkResourceError{Kind: LarkResourceErrOther, Err: err}
	}
	q := url.Values{}
	q.Set("type", resourceType)
	endpoint := fmt.Sprintf("%s/im/v1/messages/%s/resources/%s?%s",
		c.apiBase, messageID, fileKey, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", &LarkResourceError{Kind: LarkResourceErrOther, Err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", &LarkResourceError{Kind: LarkResourceErrOther, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, "", &LarkResourceError{
			Kind: LarkResourceErrPermissionDenied,
			Err:  fmt.Errorf("lark resource: %d", resp.StatusCode),
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", &LarkResourceError{Kind: LarkResourceErrNotFound, Err: errors.New("not found")}
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// Lark wraps API-level errors as JSON even on the resource
		// endpoint when the request itself is malformed. Code 99991663
		// (and the surrounding family) means "missing im:resource
		// scope" — surface that as PermissionDenied so the caller can
		// ask the admin to grant the scope rather than retrying.
		var apiErr struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Code != 0 {
			if isLarkPermissionCode(apiErr.Code) {
				return nil, "", &LarkResourceError{
					Kind: LarkResourceErrPermissionDenied,
					Err:  fmt.Errorf("lark resource: code=%d msg=%s", apiErr.Code, apiErr.Msg),
				}
			}
		}
		return nil, "", &LarkResourceError{
			Kind: LarkResourceErrOther,
			Err:  fmt.Errorf("lark resource: status=%d body=%s", resp.StatusCode, string(raw)),
		}
	}

	limit := maxBytes
	if limit <= 0 || limit > LarkResourceDownloadCap {
		limit = LarkResourceDownloadCap
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if readErr != nil {
		return nil, "", &LarkResourceError{Kind: LarkResourceErrOther, Err: readErr}
	}
	if int64(len(body)) > limit {
		return nil, "", &LarkResourceError{
			Kind: LarkResourceErrTooLarge,
			Err:  fmt.Errorf("resource body exceeds %d bytes", limit),
		}
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// isLarkPermissionCode returns true for Lark API codes that mean
// "missing scope" or "no permission". The exact codes are documented
// in Lark's open API errata; we list the ones we've actually seen
// here and treat unknown 99xx codes optimistically as "other" so a
// future code addition degrades silently rather than incorrectly
// posting a permission-warning bot reply.
func isLarkPermissionCode(code int) bool {
	switch code {
	case 99991663, // im:resource scope missing
		99991661, // im:message scope missing
		99991668: // permission denied (generic)
		return true
	}
	return false
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
		text, atts := parseLarkMessageBody(item.MsgType, item.Body.Content)
		msgs = append(msgs, LarkThreadMessage{
			MessageID:    item.MessageID,
			SenderOpenID: item.Sender.ID,
			CreatedAt:    parseLarkCreateTime(item.CreateTime),
			Text:         text,
			Attachments:  atts,
		})
	}
	return msgs, nil
}

// parseLarkMessageBody unpacks one message envelope into a (text,
// attachments) pair. Each msg_type Lark sends has a different content
// shape; we only decode the ones the §6.5 thread → issue flow knows
// how to handle:
//
//   text   — body.content is `{"text":"..."}`. No attachments.
//   image  — body.content is `{"image_key":"img_..."}`. Single
//            attachment; mime is image/* (Lark doesn't disclose the
//            exact subtype on the list endpoint, so we leave MimeType
//            empty and let the downloader read it from Content-Type).
//   file / media — body.content is `{"file_key":"file_...",
//            "file_name":"...", "file_size":..., "type":"pdf"}`.
//            type hints the extension; size is a hint, not authoritative.
//
// Unknown msg_types (post, sticker, audio, ...) produce empty text and
// nil attachments — the message still appears in the transcript via
// MessageID but the body is not lifted into the issue.
func parseLarkMessageBody(msgType, content string) (string, []LarkMessageAttachment) {
	if content == "" {
		return "", nil
	}
	switch msgType {
	case "text":
		var inner struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(content), &inner); err == nil {
			return inner.Text, nil
		}
		return "", nil

	case "image":
		var inner struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(content), &inner); err != nil || inner.ImageKey == "" {
			return "", nil
		}
		return "", []LarkMessageAttachment{{
			FileKey:      inner.ImageKey,
			ResourceType: "image",
		}}

	case "file", "media":
		var inner struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
			FileSize int64  `json:"file_size"`
			MimeType string `json:"mime_type"`
		}
		if err := json.Unmarshal([]byte(content), &inner); err != nil || inner.FileKey == "" {
			return "", nil
		}
		return "", []LarkMessageAttachment{{
			FileKey:      inner.FileKey,
			ResourceType: "file",
			Filename:     inner.FileName,
			MimeType:     inner.MimeType,
			SizeHint:     inner.FileSize,
		}}
	}
	return "", nil
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
