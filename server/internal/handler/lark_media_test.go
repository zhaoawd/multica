package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// §14.1.3 thread media → issue attachment integration tests.
//
// These exercise the full @bot 创建任务 flow with attachments:
// Lark thread list returns an image/file message, the resource
// download endpoint returns bytes (or 403 / 404 in the negative
// cases), and we verify the attachment row, the perm-warning
// reply, and the inline placeholder lines.

// larkMediaFakeServer is a fuller mock than installFakeLarkSlashServer
// — it serves both the thread-list path (with attachment messages)
// and the resource-download path with a per-key script.
type larkMediaFakeServer struct {
	srv *httptest.Server

	// Per-file_key scripted responses for the resources endpoint.
	resources map[string]larkMediaScriptedResource

	// thread is the list-endpoint payload returned for every /im/v1/messages
	// GET. Populated by callers via WithThreadMessages.
	thread []map[string]any

	mu      sync.Mutex
	replies []larkCapturedReply
}

type larkMediaScriptedResource struct {
	Body        []byte
	ContentType string
	StatusCode  int
	APICode     int // optional non-2xx with embedded Lark error code
}

func newLarkMediaFakeServer(t *testing.T) *larkMediaFakeServer {
	t.Helper()
	f := &larkMediaFakeServer{
		resources: map[string]larkMediaScriptedResource{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tenant_access_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "tk", "expire": 7200,
			})
			return

		case strings.Contains(r.URL.Path, "/resources/"):
			// /im/v1/messages/{message_id}/resources/{file_key}
			parts := strings.Split(r.URL.Path, "/")
			fileKey := parts[len(parts)-1]
			scripted, ok := f.resources[fileKey]
			if !ok {
				http.Error(w, "no script for "+fileKey, http.StatusNotFound)
				return
			}
			if scripted.APICode != 0 {
				w.WriteHeader(scripted.StatusCode)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": scripted.APICode, "msg": "scripted"})
				return
			}
			status := scripted.StatusCode
			if status == 0 {
				status = http.StatusOK
			}
			if scripted.ContentType != "" {
				w.Header().Set("Content-Type", scripted.ContentType)
			}
			w.WriteHeader(status)
			_, _ = w.Write(scripted.Body)
			return

		case strings.HasSuffix(r.URL.Path, "/reply"):
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/reply"), "/")
			messageID := parts[len(parts)-1]
			var p struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			text := extractTextField(p.Content)
			f.mu.Lock()
			f.replies = append(f.replies, larkCapturedReply{MessageID: messageID, Text: text})
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
			return

		case r.URL.Path == "/im/v1/messages":
			// Thread list endpoint.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"items": f.thread},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))

	cfg := service.LarkConfig{AppID: "a", AppSecret: "b", VerificationToken: "verify_test_token", EncryptKey: "encrypt_test_key"}
	client := service.NewLarkClient(cfg)
	service.SetAPIBaseForTest(client, f.srv.URL)
	// Swap LarkThread and LarkMedia atomically. The media service is
	// what §14.1.3 exercises; LarkThread is needed for the @bot
	// create-issue path itself.
	prevThread := testHandler.LarkThread
	prevMedia := testHandler.LarkMedia
	prevStorage := testHandler.Storage
	if testHandler.Storage == nil {
		testHandler.Storage = &mockStorage{}
	}
	testHandler.LarkThread = service.NewLarkThreadService(testHandler.Queries, testPool, testHandler.Bus, client)
	testHandler.LarkMedia = service.NewLarkMediaService(testHandler.Queries, client, testHandler.Storage)
	t.Cleanup(func() {
		testHandler.LarkThread = prevThread
		testHandler.LarkMedia = prevMedia
		testHandler.Storage = prevStorage
		f.srv.Close()
	})
	return f
}

func (f *larkMediaFakeServer) Replies() []larkCapturedReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]larkCapturedReply, len(f.replies))
	copy(out, f.replies)
	return out
}

// withThreadMessages installs a list-endpoint payload. Each entry is a
// map shaped like one Lark list response item (msg_type + body +
// message_id). Pass through json.RawMessage to keep the helper
// flexible across text / image / file shapes.
func (f *larkMediaFakeServer) withThreadMessages(items ...map[string]any) {
	f.thread = items
}

// minimalImageMessageItem returns a list-response item for one image
// message with the given file_key and message_id.
func minimalImageMessageItem(messageID, imageKey string) map[string]any {
	return map[string]any{
		"message_id":  messageID,
		"create_time": "1747555200000",
		"msg_type":    "image",
		"sender":      map[string]any{"id": "ou_sender", "id_type": "open_id"},
		"body":        map[string]any{"content": `{"image_key":"` + imageKey + `"}`},
	}
}

// minimalFileMessageItem returns a list-response item for one file
// message with explicit size hint and mime type.
func minimalFileMessageItem(messageID, fileKey, filename, mime string, size int64) map[string]any {
	content := map[string]any{
		"file_key":  fileKey,
		"file_name": filename,
		"file_size": size,
		"mime_type": mime,
	}
	contentBytes, _ := json.Marshal(content)
	return map[string]any{
		"message_id":  messageID,
		"create_time": "1747555200000",
		"msg_type":    "file",
		"sender":      map[string]any{"id": "ou_sender", "id_type": "open_id"},
		"body":        map[string]any{"content": string(contentBytes)},
	}
}

// fakePNGBytes returns a tiny but valid-shape PNG header so any
// downstream content-sniffing libraries see it as a real image. The
// content here is opaque to multica's storage and the attachment
// row only carries the bytes verbatim.
func fakePNGBytes() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	}
}

func TestLarkMedia_ImageAttachmentLandsAndRecordsHash(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := newLarkMediaFakeServer(t)

	const chatID = "oc_media_image_happy"
	const openID = "ou_media_image_user"
	const triggerMsgID = "om_media_trigger"
	const threadID = "thread_media_1"
	const imgKey1 = "img_unique_one"
	const imgKey2 = "img_unique_two_same_bytes"

	seedLarkChatBinding(t, chatID)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	// Thread carries two image messages with identical bytes. After
	// the P1 #1 fix each attachment row owns its own blob URL — URL
	// reuse is unsafe until a blob/ref-count layer lands, because
	// every delete path (file.go / issue.go / comment.go) deletes
	// the row's URL unconditionally. The shared sha256 is still
	// recorded on both rows so a future ref-counted layer can pick
	// up the dedup signal from the existing data.
	pngBytes := fakePNGBytes()
	fake.withThreadMessages(
		minimalImageMessageItem("om_msg_1", imgKey1),
		minimalImageMessageItem("om_msg_2", imgKey2),
	)
	fake.resources[imgKey1] = larkMediaScriptedResource{Body: pngBytes, ContentType: "image/png"}
	fake.resources[imgKey2] = larkMediaScriptedResource{Body: pngBytes, ContentType: "image/png"}

	body := buildLarkV2MessageEvent(openID, chatID, threadID, triggerMsgID,
		"@_user_1 创建任务 thread with screenshots")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}

	// Find the created issue.
	var issueID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id FROM issue WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT 1`,
		testWorkspaceID).Scan(&issueID); err != nil {
		t.Fatalf("find issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(),
			`DELETE FROM attachment WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(),
			`DELETE FROM issue WHERE id = $1`, issueID)
	})

	var attCount int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM attachment WHERE issue_id = $1`, issueID).Scan(&attCount); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attCount != 2 {
		t.Fatalf("expected 2 attachment rows, got %d", attCount)
	}

	// Each row owns its blob (no cross-row URL aliasing): delete-one
	// safety is what this asserts. The sha256 column still matches
	// across rows so a future ref-counted dedup can read it.
	var distinctURLs int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(DISTINCT url) FROM attachment WHERE issue_id = $1`, issueID).Scan(&distinctURLs); err != nil {
		t.Fatalf("count distinct urls: %v", err)
	}
	if distinctURLs != 2 {
		t.Fatalf("expected each attachment to own its blob URL (2 distinct), got %d", distinctURLs)
	}

	var distinctHashes int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(DISTINCT content_sha256) FROM attachment WHERE issue_id = $1`, issueID).Scan(&distinctHashes); err != nil {
		t.Fatalf("count distinct hashes: %v", err)
	}
	if distinctHashes != 1 {
		t.Fatalf("identical bytes should share sha256, got %d distinct hashes", distinctHashes)
	}

	// Provenance column is populated.
	var sourcePrefix string
	if err := testPool.QueryRow(context.Background(),
		`SELECT source FROM attachment WHERE issue_id = $1 ORDER BY created_at ASC LIMIT 1`,
		issueID).Scan(&sourcePrefix); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if !strings.HasPrefix(sourcePrefix, "lark_thread:"+chatID+":") {
		t.Errorf("source should encode chat+message, got %q", sourcePrefix)
	}

	// Reply chain: no permission warning.
	for _, rp := range fake.Replies() {
		if strings.Contains(rp.Text, "im:resource") {
			t.Errorf("unexpected perm warning reply: %q", rp.Text)
		}
	}
}

func TestLarkMedia_OversizedFileSkippedWithNotice(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := newLarkMediaFakeServer(t)

	const chatID = "oc_media_oversized"
	const openID = "ou_media_oversized_user"
	const triggerMsgID = "om_oversize_trigger"
	const threadID = "thread_oversize"
	const fileKey = "file_oversize"

	seedLarkChatBinding(t, chatID)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	// SizeHint over the 10 MB cap — short-circuits the download
	// before any bytes are fetched. The resources endpoint won't be
	// hit, but we still script it to fail loudly if the code ever
	// regresses and calls it anyway.
	fake.withThreadMessages(
		minimalFileMessageItem(triggerMsgID, fileKey, "huge.pdf",
			"application/pdf", 20*1024*1024),
	)
	fake.resources[fileKey] = larkMediaScriptedResource{StatusCode: http.StatusBadRequest}

	body := buildLarkV2MessageEvent(openID, chatID, threadID, "om_create",
		"@_user_1 创建任务 oversized file thread")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}

	// Issue exists, attachment row does NOT.
	var issueID string
	var description string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id, COALESCE(description,'') FROM issue WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT 1`,
		testWorkspaceID).Scan(&issueID, &description); err != nil {
		t.Fatalf("find issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(),
			`DELETE FROM attachment WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(),
			`DELETE FROM issue WHERE id = $1`, issueID)
	})

	var attCount int
	testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM attachment WHERE issue_id = $1`, issueID).Scan(&attCount)
	if attCount != 0 {
		t.Fatalf("oversized file should not produce an attachment row, got %d", attCount)
	}

	// Inline placeholder is appended to the issue description.
	if !strings.Contains(description, "[oversized attachment: huge.pdf") {
		t.Errorf("issue description missing oversized notice: %q", description)
	}
}

func TestLarkMedia_OversizedImageNoSizeHintRejectedAtBoundedLimit(t *testing.T) {
	// §14.1.3 P1 #3 regression guard: an image message has SizeHint==0
	// in the list envelope. Before bounding the download, the client
	// would read up to 64 MB before the post-download cap rejected it.
	// With the bounded download we ask for at most LarkMediaMaxFileBytes+1
	// bytes; the fake server here returns 12 MB so the LimitReader trips
	// at 10 MB+1 and the media service maps TooLarge → oversized notice.
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := newLarkMediaFakeServer(t)

	const chatID = "oc_media_huge_image"
	const openID = "ou_media_huge_image_user"
	const triggerMsgID = "om_huge_image_trigger"
	const threadID = "thread_huge_image"
	const imgKey = "img_huge_no_hint"

	seedLarkChatBinding(t, chatID)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	fake.withThreadMessages(minimalImageMessageItem("om_msg_huge", imgKey))
	// 12 MB body — over the 10 MB per-file cap but well under the
	// 64 MB client hard cap. Without bounded download we'd buffer all
	// 12 MB; with it the LimitReader stops at 10 MB+1.
	fake.resources[imgKey] = larkMediaScriptedResource{
		Body:        bytes.Repeat([]byte{0x89}, 12*1024*1024),
		ContentType: "image/png",
	}

	body := buildLarkV2MessageEvent(openID, chatID, threadID, triggerMsgID,
		"@_user_1 创建任务 huge image thread")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}

	var issueID string
	var description string
	if err := testPool.QueryRow(context.Background(),
		`SELECT id, COALESCE(description,'') FROM issue WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT 1`,
		testWorkspaceID).Scan(&issueID, &description); err != nil {
		t.Fatalf("find issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(),
			`DELETE FROM attachment WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(),
			`DELETE FROM issue WHERE id = $1`, issueID)
	})

	var attCount int
	testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM attachment WHERE issue_id = $1`, issueID).Scan(&attCount)
	if attCount != 0 {
		t.Fatalf("oversized image should not persist, got %d rows", attCount)
	}
	if !strings.Contains(description, "[oversized attachment:") {
		t.Errorf("issue description missing oversized notice: %q", description)
	}
}

func TestLarkMedia_PermissionDeniedEmitsThrottledWarning(t *testing.T) {
	if testHandler == nil {
		t.Skip("DB not available")
	}
	withLarkWebhookEnv(t)
	cleanupLarkUserLink(t)
	fake := newLarkMediaFakeServer(t)

	const chatID = "oc_media_perm_denied"
	const openID = "ou_media_perm_user"
	const triggerMsgID = "om_perm_trigger"
	const threadID = "thread_perm_denied"
	const imgKey = "img_perm_denied"

	seedLarkChatBinding(t, chatID)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO lark_user_link (user_id, lark_open_id) VALUES ($1, $2)`,
		testUserID, openID); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	fake.withThreadMessages(minimalImageMessageItem("om_msg_1", imgKey))
	fake.resources[imgKey] = larkMediaScriptedResource{StatusCode: http.StatusForbidden}

	body := buildLarkV2MessageEvent(openID, chatID, threadID, triggerMsgID,
		"@_user_1 创建任务 permission denied thread")
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/lark", bytes.NewReader(body))
	signLarkRequest(req, body)
	rr := httptest.NewRecorder()
	testHandler.HandleLarkWebhook(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}

	// One of the reply payloads must be the perm-warning line.
	var sawWarning bool
	for _, rp := range fake.Replies() {
		if strings.Contains(rp.Text, "im:resource") {
			sawWarning = true
			break
		}
	}
	if !sawWarning {
		t.Fatalf("expected im:resource warning reply, got %+v", fake.Replies())
	}

	// And the binding row records the timestamp so the next failure
	// doesn't re-warn.
	var stampedAt *string
	testPool.QueryRow(context.Background(),
		`SELECT last_perm_warning_at::text FROM lark_workspace_binding WHERE workspace_id = $1`,
		testWorkspaceID).Scan(&stampedAt)
	if stampedAt == nil || *stampedAt == "" {
		t.Errorf("last_perm_warning_at not stamped, got %v", stampedAt)
	}
}
