// Package service — Lark (Feishu) document content fetcher (P3.A).
//
// Used at task-dispatch time to expand Lark doc URLs referenced from an
// issue body or comments into plain-text content the agent can read. The
// result rides on the claim response as `linked_docs`; the daemon embeds
// each LinkedDoc into the task prompt so the agent treats the doc body as
// part of its task description.
//
// Design constraints (see LARK_INTEGRATION_DESIGN.md §6.3):
//   - No cache. One fetch per claim per URL; re-running on resume / redispatch
//     is the explicit way users get the latest doc revision into the agent.
//   - Failures degrade silently — empty Content + a populated Error string
//     rather than a hard claim error. The agent sees `[doc unavailable: ...]`
//     in the prompt rather than missing context.
//   - No SDK pull-in. We continue to talk Lark's HTTP API directly through
//     the existing tenant-access-token cache on *LarkClient.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

// Tuning knobs for claim-time doc expansion. These bound the worst-case
// claim latency a single issue can incur — a pathological issue with many
// Lark URLs and a slow Lark API must not pin the daemon's claim poll.
const (
	// MaxDocsPerClaim caps the number of URLs expanded for one claim.
	// Worst-case latency = MaxDocsPerClaim * DocFetchTimeout, so any change
	// here should consider that product (currently 25s).
	MaxDocsPerClaim = 5

	// DocFetchTimeout is the per-URL HTTP timeout. Lark p99 is well under
	// 2s for docx raw_content; 5s gives headroom for the wiki resolve step
	// plus regional latency from CN tenants.
	DocFetchTimeout = 5 * time.Second

	// CommentsScanLimit caps how many comments we scan for doc URLs at
	// claim time. p99 issue has ~30 comments; 200 covers the long tail
	// without dragging the claim into a multi-second DB read.
	CommentsScanLimit = 200
)

// LinkedDoc is the wire shape of a Lark doc reference. The agent claim
// response carries a slice of these; the daemon embeds them into the task
// prompt verbatim. Exactly one of Content / Error is populated.
//
// Error vocabulary (kept small so the daemon prompt template is stable):
//   - ""           → success, Content holds the doc body
//   - "forbidden"  → tenant_access_token lacked permission for the doc
//   - "not_found"  → the doc / wiki node does not exist
//   - "unavailable" → integration disabled, unsupported URL kind, or other
//     transient API failure (network, 5xx, malformed JSON, etc.)
type LinkedDoc struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

// docURLRegex matches Lark / Feishu document URLs of the common shapes:
//
//	https://<tenant>.feishu.cn/{docx|docs|wiki|sheets|base|file}/<token>
//	https://<tenant>.larksuite.com/...
//	https://<tenant>.feishu.net/...   (rare; some SaaS deployments)
//
// We deliberately capture sheets / base / file too so ExtractDocURLs can
// surface them as "linked docs we recognise but won't expand"; Fetch maps
// those to Error="unavailable".
var docURLRegex = regexp.MustCompile(
	`https?://[a-zA-Z0-9.-]+\.(?:feishu\.cn|larksuite\.com|feishu\.net)/(docx|docs|wiki|sheets|base|file)/([a-zA-Z0-9_-]+)`)

// ExtractDocURLs returns the unique Lark doc URLs that appear in text, in
// order of first appearance. Markdown link syntax (`[label](url)`) does not
// disrupt detection — the regex matches the URL substring directly.
func ExtractDocURLs(text string) []string {
	if text == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, m := range docURLRegex.FindAllString(text, -1) {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// parseDocURL extracts the (kind, token) tuple from a Lark URL. Returns
// ok=false when the URL doesn't match a known Lark doc pattern. Exposed for
// testability — Fetch uses it internally.
func parseDocURL(url string) (kind, token string, ok bool) {
	m := docURLRegex.FindStringSubmatch(url)
	if len(m) != 3 {
		return "", "", false
	}
	return m[1], m[2], true
}

// LarkDocs fetches Lark doc content for inclusion in the agent task prompt.
// Constructed once at server startup; safe to call when the Lark integration
// is unconfigured (Fetch will then synthesise an "unavailable" LinkedDoc).
type LarkDocs struct {
	client *LarkClient
	cfg    LarkConfig
}

// NewLarkDocs wraps a LarkClient. Pass nil when the integration is disabled
// so callers don't need a separate nil-check.
func NewLarkDocs(client *LarkClient) *LarkDocs {
	var cfg LarkConfig
	if client != nil {
		cfg = client.cfg
	}
	return &LarkDocs{client: client, cfg: cfg}
}

// Configured reports whether the Lark integration env is fully populated.
func (d *LarkDocs) Configured() bool { return d.cfg.Configured() }

// Fetch resolves a Lark doc URL and returns its plain-text content as a
// LinkedDoc. Routing:
//
//   - docx → /open-apis/docx/v1/documents/{id}/raw_content
//   - wiki → resolve obj_token via /open-apis/wiki/v2/spaces/get_node, then docx
//
// Unsupported kinds (sheets, base, file, legacy docs) and any failure mode
// degrade to Error="unavailable" / "forbidden" / "not_found" with empty
// Content so the agent prompt template still renders a stable placeholder.
func (d *LarkDocs) Fetch(ctx context.Context, url string) LinkedDoc {
	doc := LinkedDoc{URL: url}
	if d == nil || d.client == nil || !d.Configured() {
		doc.Error = "unavailable"
		return doc
	}
	kind, token, ok := parseDocURL(url)
	if !ok {
		doc.Error = "unavailable"
		return doc
	}

	var docxID string
	switch kind {
	case "docx":
		docxID = token
	case "wiki":
		resolved, err := d.resolveWikiNode(ctx, token)
		if err != nil {
			doc.Error = fetchErrorCode(err)
			return doc
		}
		docxID = resolved
	default:
		// sheets / base / file / legacy docs — recognised but not expanded
		// in P3.A. Adding them later is a matter of an additional API path,
		// not a structural change.
		doc.Error = "unavailable"
		return doc
	}

	content, err := d.fetchDocxRaw(ctx, docxID)
	if err != nil {
		doc.Error = fetchErrorCode(err)
		return doc
	}
	doc.Content = content
	return doc
}

// ── Lark HTTP plumbing ───────────────────────────────────────────────────

// larkFetchError carries the Lark API code + HTTP status so callers can map
// it to the LinkedDoc.Error vocabulary without inspecting raw strings.
type larkFetchError struct {
	httpStatus int
	code       int
	msg        string
}

func (e *larkFetchError) Error() string {
	return fmt.Sprintf("lark: http=%d code=%d msg=%s", e.httpStatus, e.code, e.msg)
}

// fetchErrorCode maps a fetch error to the LinkedDoc.Error vocabulary.
// Everything that doesn't look explicitly like "forbidden" or "not_found"
// falls through to "unavailable" — Lark error codes are not formally
// guaranteed stable, so the conservative bucketing keeps the prompt UX
// predictable.
func fetchErrorCode(err error) string {
	var le *larkFetchError
	if errors.As(err, &le) {
		if le.httpStatus == http.StatusForbidden || isLarkForbidden(le.code) {
			return "forbidden"
		}
		if le.httpStatus == http.StatusNotFound || isLarkNotFound(le.code) {
			return "not_found"
		}
	}
	return "unavailable"
}

// isLarkForbidden / isLarkNotFound encode the small set of Lark business
// codes we've observed in the field. Anything else falls through to
// "unavailable" — these codes aren't formally documented to be stable, so
// the allow-lists stay conservative.
func isLarkForbidden(code int) bool {
	switch code {
	case 1254003, 1254040, 1254100, 1254201, 1254202, 1254203,
		1254301, 1254302, 1254400, 1254406, 1254500:
		return true
	}
	return false
}

func isLarkNotFound(code int) bool {
	switch code {
	case 1254000, 1254001, 1254404, 99991401:
		return true
	}
	return false
}

// fetchDocxRaw calls `docx/v1/documents/{id}/raw_content`. The Lark API
// returns the document body as a single UTF-8 string, with structural
// elements (headings, lists, tables) already flattened — exactly what the
// agent needs as plain prompt context.
func (d *LarkDocs) fetchDocxRaw(ctx context.Context, docxID string) (string, error) {
	token, err := d.client.tenantAccessToken(ctx)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/docx/v1/documents/%s/raw_content?lang=0", d.client.apiBase, docxID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.client.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &larkFetchError{httpStatus: resp.StatusCode, msg: string(raw)}
	}
	if out.Code != 0 {
		return "", &larkFetchError{httpStatus: resp.StatusCode, code: out.Code, msg: out.Msg}
	}
	return out.Data.Content, nil
}

// resolveWikiNode swaps a wiki token for the underlying obj_token of the
// docx the wiki entry points at. Wiki nodes can point at sheets / mindnotes /
// bitable too; only docx nodes get expanded to raw content (others bubble up
// as larkFetchError → "unavailable").
func (d *LarkDocs) resolveWikiNode(ctx context.Context, wikiToken string) (string, error) {
	token, err := d.client.tenantAccessToken(ctx)
	if err != nil {
		return "", err
	}
	endpoint := fmt.Sprintf("%s/wiki/v2/spaces/get_node?token=%s", d.client.apiBase, wikiToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.client.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Node struct {
				ObjToken string `json:"obj_token"`
				ObjType  string `json:"obj_type"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &larkFetchError{httpStatus: resp.StatusCode, msg: string(raw)}
	}
	if out.Code != 0 {
		return "", &larkFetchError{httpStatus: resp.StatusCode, code: out.Code, msg: out.Msg}
	}
	if out.Data.Node.ObjType != "docx" || out.Data.Node.ObjToken == "" {
		// Wiki nodes that point at sheets / mindnotes / bitable are out of
		// scope for raw text expansion in P3.A. Surface as "unavailable" via
		// the larkFetchError → fetchErrorCode mapping.
		return "", &larkFetchError{msg: "wiki node not docx (type=" + out.Data.Node.ObjType + ")"}
	}
	return out.Data.Node.ObjToken, nil
}
