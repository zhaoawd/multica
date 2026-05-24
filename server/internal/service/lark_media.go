package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/storage"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Lark thread media → issue attachment (LARK_INTEGRATION_DESIGN.md §14.1.3).
//
// LarkMediaService is the bounded surface that downloads attachments
// referenced by a Lark thread and persists them as multica
// attachments on the new issue. Three boundaries the design pins
// (and this service enforces):
//
//   - Bytes never reach the agent without going through multica's blob
//     storage first. The agent reads the issue's attachment URLs, not
//     Lark's. (§11 invariant 3.)
//   - Size and type limits are hard caps, not warnings. Anything that
//     exceeds them is logged in the issue body as a "[ ... ]" line so
//     the agent and reviewer can see why the asset is missing.
//   - Failures are partial: one missing attachment must not block
//     issue creation. The download path returns a structured report
//     and the caller composes a final issue body that names the
//     successes and the misses.

// Per-file size cap. The design picks 10 MB as "fits in agent context"
// and "fits in our blob storage budget without per-issue surprises".
// Files over this cap render an "[oversized attachment]" line.
const LarkMediaMaxFileBytes = 10 * 1024 * 1024

// Per-issue total cap. Even if every file is under MaxFileBytes, a
// thread with twenty screenshots would still blow the agent's context
// budget — and any blob over the limit is wasted dollars when we
// charge the workspace for storage. The first download that would
// exceed this total is skipped (and every subsequent one); earlier
// successes stay.
const LarkMediaMaxTotalBytes = 50 * 1024 * 1024

// LarkMediaAllowedMimes is the closed type whitelist. Anything not in
// here renders an "[unsupported attachment type]" line.
//
// Why not video/audio: the §14.1.3 scope ("agent context can't ingest
// them today"). Adding them later is an explicit RFC addition, not a
// silent extension. PDF is in because the docs fetcher path (§6.3)
// handles Lark cloud docs but NOT PDF attachments — the agent can
// at least read them as raw bytes via attachment.
var LarkMediaAllowedMimes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"application/pdf": true,
}

// LarkMediaService bridges Lark thread media into multica attachments.
// Holds the dependencies that are not on LarkThreadService (Storage
// is the differentiator) so that thread bridging works without
// blob storage and the media path opts in only where storage exists.
type LarkMediaService struct {
	Queries *db.Queries
	Client  *LarkClient
	Storage storage.Storage
	Log     *slog.Logger
}

// NewLarkMediaService constructs the service. Any of (Client, Storage,
// Queries) being nil yields a service whose methods are no-ops + a
// "media unavailable" report — that lets the webhook wire the
// service unconditionally and have it gracefully degrade in test
// environments without blob storage.
func NewLarkMediaService(q *db.Queries, client *LarkClient, store storage.Storage) *LarkMediaService {
	return &LarkMediaService{
		Queries: q,
		Client:  client,
		Storage: store,
		Log:     slog.Default(),
	}
}

// Configured reports whether all dependencies are wired and the
// underlying Lark client has full env. Test environments (no Storage)
// and unbound deployments (no Lark env) both fail this check.
func (s *LarkMediaService) Configured() bool {
	return s != nil &&
		s.Queries != nil &&
		s.Storage != nil &&
		s.Client != nil &&
		s.Client.cfg.Configured()
}

// LarkMediaResult is returned per attempted attachment.
//
// Status names the outcome with the §14.1.3 vocabulary so the caller
// can route each result to either the issue's attachment list (when
// Persisted) or an inline placeholder line (every other status).
//
// Notice describes what to render inline when Persisted is false:
//
//	oversized        → "[oversized attachment: <name> <size>]"
//	unsupported      → "[unsupported attachment type: <mime>]"
//	unavailable      → "[attachment unavailable: <name>]"
//	permission       → triggers the throttled bot perm-warning reply;
//	                   no inline placeholder (the reply is the surface)
//	limit_exhausted  → "[issue attachment budget reached, skipping <name>]"
type LarkMediaResult struct {
	MessageID    string
	Filename     string
	MimeType     string
	SizeBytes    int64
	Persisted    bool
	AttachmentID pgtype.UUID
	Status       string // see Notice doc above
	Notice       string
}

// LarkMediaReport collects results across one issue's worth of
// downloads. The Permission* fields are set when ANY result hit
// permission_denied — the caller surfaces that as a one-time bot
// reply in the thread (throttled via lark_workspace_binding).
type LarkMediaReport struct {
	Results          []LarkMediaResult
	PermissionDenied bool
	TotalBytes       int64
}

// PersistedAttachmentIDs returns the IDs of attachments that landed
// successfully. The webhook may use these to surface the count in
// the @bot reply ("created MUL-12 with 2 attachments").
func (r *LarkMediaReport) PersistedAttachmentIDs() []pgtype.UUID {
	ids := make([]pgtype.UUID, 0, len(r.Results))
	for _, res := range r.Results {
		if res.Persisted {
			ids = append(ids, res.AttachmentID)
		}
	}
	return ids
}

// InlineNotices returns the placeholder lines for failed / skipped
// attachments. The caller appends them to the issue description so
// the agent reading the issue understands why an image referenced in
// the original thread is not in the attachment list.
func (r *LarkMediaReport) InlineNotices() []string {
	out := make([]string, 0)
	for _, res := range r.Results {
		if res.Notice != "" {
			out = append(out, res.Notice)
		}
	}
	return out
}

// DownloadAndAttach iterates the message attachments in tc, downloads
// each (subject to type / size / total caps), and persists each
// successful download as a multica attachment row linked to issue.
//
// The function never returns an error: every failure is recorded in
// the per-attachment result. This matches the design's "one missing
// attachment must not block issue creation" rule — the issue exists,
// the bot's reply mentions what landed, and the misses are surfaced
// inline via Notice strings the caller injects into the issue body.
//
// Caller responsibility:
//   - issueID must reference a row that already exists.
//   - tc is the LarkThreadContext used to create the issue (so the
//     provenance "lark_thread:<chat_id>:<message_id>" matches the
//     bridge row).
//   - workspaceID and uploader are propagated to attachment rows.
func (s *LarkMediaService) DownloadAndAttach(
	ctx context.Context,
	workspaceID pgtype.UUID,
	uploaderType string,
	uploaderID pgtype.UUID,
	issueID pgtype.UUID,
	tc *LarkThreadContext,
) LarkMediaReport {
	var report LarkMediaReport
	if !s.Configured() || tc == nil {
		return report
	}

	for _, msg := range tc.ThreadMessages {
		for _, att := range msg.Attachments {
			result := s.processOneAttachment(ctx, workspaceID, uploaderType, uploaderID, issueID, tc.ChatID, msg.MessageID, att, &report)
			report.Results = append(report.Results, result)
			if result.Status == "permission" {
				report.PermissionDenied = true
			}
		}
	}
	return report
}

// processOneAttachment does the per-blob work: download, size-check,
// mime-check, dedup, upload, attachment row. Returns the structured
// result; the caller updates the cumulative budget on success.
func (s *LarkMediaService) processOneAttachment(
	ctx context.Context,
	workspaceID pgtype.UUID,
	uploaderType string,
	uploaderID pgtype.UUID,
	issueID pgtype.UUID,
	chatID, messageID string,
	att LarkMessageAttachment,
	report *LarkMediaReport,
) LarkMediaResult {
	displayName := att.Filename
	if displayName == "" {
		displayName = larkSyntheticAttachmentName(att)
	}
	r := LarkMediaResult{
		MessageID: messageID,
		Filename:  displayName,
		MimeType:  att.MimeType,
		SizeBytes: att.SizeHint,
	}

	// Pre-check the size hint when Lark provided one. Saves a round-trip
	// on the file path where the message envelope carries file_size;
	// image messages have SizeHint==0 so this is a no-op and the post-
	// download cap below catches oversize images.
	if att.SizeHint > 0 && att.SizeHint > LarkMediaMaxFileBytes {
		r.Status = "oversized"
		r.Notice = fmt.Sprintf("[oversized attachment: %s %s]", displayName, formatByteSize(att.SizeHint))
		return r
	}

	// Pre-check envelope mime when known. File messages carry mime_type
	// in the list response; rejecting unsupported types here saves a
	// download we'd discard at the whitelist check below. Image
	// messages have an empty envelope mime — those still go through
	// the post-download check using the resource's Content-Type.
	if att.MimeType != "" && !LarkMediaAllowedMimes[att.MimeType] {
		r.Status = "unsupported"
		r.Notice = fmt.Sprintf("[unsupported attachment type: %s]", att.MimeType)
		return r
	}

	// Hard budget gate. Once the persisted total has reached the
	// per-issue cap, EVERY subsequent attachment is skipped without
	// downloading — image messages have SizeHint==0 so the old
	// "only skip when SizeHint > 0" branch would otherwise keep
	// downloading bytes only to drop them post-fetch. Refuse the
	// fetch instead so we don't burn network + memory on payloads
	// we have already committed to dropping.
	if report.TotalBytes >= LarkMediaMaxTotalBytes {
		r.Status = "limit_exhausted"
		r.Notice = fmt.Sprintf("[issue attachment budget reached, skipping %s]", displayName)
		return r
	}
	// Honest envelope size also lets us pre-skip the next attachment
	// that would push us over (instead of fetching it just to discard).
	if att.SizeHint > 0 && report.TotalBytes+att.SizeHint > LarkMediaMaxTotalBytes {
		r.Status = "limit_exhausted"
		r.Notice = fmt.Sprintf("[issue attachment budget reached, skipping %s]", displayName)
		return r
	}

	// Bound the download. Without this, an image message (SizeHint==0)
	// pointing at a 64 MB blob would buffer the full 64 MB only to be
	// rejected by the post-download 10 MB cap below. Pick the tighter
	// of "per-file cap" and "remaining per-issue budget" so the read
	// stops at whichever limit applies first. The +1 is so the client
	// can detect overflow vs. exactly-at-limit.
	perFileLimit := int64(LarkMediaMaxFileBytes)
	remainingBudget := LarkMediaMaxTotalBytes - report.TotalBytes
	downloadLimit := perFileLimit
	if remainingBudget < downloadLimit {
		downloadLimit = remainingBudget
	}

	body, contentType, derr := s.Client.DownloadMessageResource(ctx, messageID, att.FileKey, att.ResourceType, downloadLimit)
	if derr != nil {
		switch derr.Kind {
		case LarkResourceErrPermissionDenied:
			r.Status = "permission"
			// No inline notice: the design routes this to a one-time
			// thread reply asking the admin to grant the scope, not
			// per-message placeholders in the issue body.
			s.Log.Warn("lark media: permission denied", "message_id", messageID, "file_key", att.FileKey)
		case LarkResourceErrNotFound:
			r.Status = "unavailable"
			r.Notice = fmt.Sprintf("[attachment unavailable: %s]", displayName)
		case LarkResourceErrTooLarge:
			// Two cases collapse into TooLarge — distinguish them by
			// which budget was the binding limit. If the per-file cap
			// was tighter, this is an oversized file. If the remaining
			// per-issue budget was tighter, the issue already has too
			// many attachments to fit this one (limit_exhausted).
			if remainingBudget < perFileLimit {
				r.Status = "limit_exhausted"
				r.Notice = fmt.Sprintf("[issue attachment budget reached, skipping %s]", displayName)
			} else {
				r.Status = "oversized"
				r.Notice = fmt.Sprintf("[oversized attachment: %s over %s]", displayName, formatByteSize(perFileLimit))
			}
		default:
			r.Status = "unavailable"
			r.Notice = fmt.Sprintf("[attachment unavailable: %s]", displayName)
			s.Log.Warn("lark media: download failed", "err", derr.Err, "message_id", messageID, "file_key", att.FileKey)
		}
		return r
	}

	r.SizeBytes = int64(len(body))
	r.MimeType = pickMime(att.MimeType, contentType)

	if r.SizeBytes > LarkMediaMaxFileBytes {
		r.Status = "oversized"
		r.Notice = fmt.Sprintf("[oversized attachment: %s %s]", displayName, formatByteSize(r.SizeBytes))
		return r
	}
	if !LarkMediaAllowedMimes[r.MimeType] {
		r.Status = "unsupported"
		r.Notice = fmt.Sprintf("[unsupported attachment type: %s]", r.MimeType)
		return r
	}
	if report.TotalBytes+r.SizeBytes > LarkMediaMaxTotalBytes {
		r.Status = "limit_exhausted"
		r.Notice = fmt.Sprintf("[issue attachment budget reached, skipping %s]", displayName)
		return r
	}

	// Compute the content hash for audit + future ref-counted dedup,
	// but do NOT reuse another attachment's blob URL. The existing
	// delete paths (handler/file.go DeleteAttachment, issue.go
	// DeleteIssue, comment.go DeleteComment) all call deleteS3Object
	// on the row's URL without ref counting — sharing a URL across
	// rows would orphan one attachment when the other is deleted.
	// Storage-level dedup is deferred to a future blob-ref-count
	// layer; until then each attachment owns its bytes.
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	key := larkAttachmentStorageKey(workspaceID, att, displayName)
	uploadName := displayName
	if uploadName == "" {
		uploadName = path.Base(key)
	}
	blobURL, err := s.Storage.Upload(ctx, key, body, r.MimeType, uploadName)
	if err != nil {
		r.Status = "unavailable"
		r.Notice = fmt.Sprintf("[attachment unavailable: %s]", displayName)
		s.Log.Warn("lark media: storage upload failed", "err", err, "file_key", att.FileKey)
		return r
	}

	row, err := s.Queries.CreateAttachment(ctx, db.CreateAttachmentParams{
		ID:            pgtype.UUID{Bytes: uuid.New(), Valid: true},
		WorkspaceID:   workspaceID,
		IssueID:       issueID,
		UploaderType:  uploaderType,
		UploaderID:    uploaderID,
		Filename:      displayName,
		Url:           blobURL,
		ContentType:   r.MimeType,
		SizeBytes:     r.SizeBytes,
		ContentSha256: pgtype.Text{String: digest, Valid: true},
		Source: pgtype.Text{
			String: fmt.Sprintf("lark_thread:%s:%s", chatID, messageID),
			Valid:  true,
		},
	})
	if err != nil {
		r.Status = "unavailable"
		r.Notice = fmt.Sprintf("[attachment unavailable: %s]", displayName)
		s.Log.Warn("lark media: attachment row insert failed", "err", err)
		return r
	}

	report.TotalBytes += r.SizeBytes
	r.Persisted = true
	r.Status = "ok"
	r.AttachmentID = row.ID
	return r
}

// EmitPermissionWarning posts the §14.1.3 throttled bot reply asking
// the admin to grant the im:resource scope, AND stamps the binding's
// last_perm_warning_at so the same warning isn't re-posted on every
// subsequent message. Idempotent and safe to call when no warning is
// needed (it short-circuits on Configured()).
//
// rootMessageID is the thread root to reply into — we anchor on the
// root so a thread that re-hits permission denied later still pins
// the warning visibly under the original thread, not buried under a
// later message.
func (s *LarkMediaService) EmitPermissionWarning(ctx context.Context, workspaceID pgtype.UUID, rootMessageID string) {
	if !s.Configured() || rootMessageID == "" {
		return
	}

	// Read the binding's last_perm_warning_at; skip when we've posted
	// the warning in the recent past. The threshold is generous —
	// once per binding ever is a reasonable simplification, since
	// the admin granting the scope is a one-time action.
	binding, err := s.Queries.GetLarkWorkspaceBinding(ctx, workspaceID)
	if err != nil {
		// Without a binding we can't reply anyway. Log and bail.
		s.Log.Warn("lark media: binding lookup for perm warning failed", "err", err)
		return
	}
	if binding.LastPermWarningAt.Valid {
		// Already warned. Don't spam.
		return
	}

	text := "Multica bot 缺少下载附件的权限。请管理员在 Lark 开放平台为应用补齐 `im:resource` 权限，之后重新运行 `@bot 创建任务`。"
	if err := s.Client.ReplyToMessage(ctx, rootMessageID, text); err != nil {
		s.Log.Warn("lark media: perm warning reply failed", "err", err)
		return
	}
	if err := s.Queries.MarkLarkBindingPermWarning(ctx, workspaceID); err != nil {
		// Logged but not retried — the reply already landed, so the
		// human-visible state is correct. Worst case we warn twice if
		// this update fails on a subsequent attachment.
		s.Log.Warn("lark media: stamp perm warning failed", "err", err)
	}
}

// larkAttachmentStorageKey builds the storage object key for a Lark
// attachment. Matches the convention `workspaces/<ws>/lark/<uuid><ext>`
// — the lark/ subdir is a quick visual signal in storage browsers
// that the blob came from the bridge rather than direct upload.
func larkAttachmentStorageKey(workspaceID pgtype.UUID, att LarkMessageAttachment, displayName string) string {
	id := uuid.New().String()
	ext := larkAttachmentExt(att, displayName)
	wsHex := pgUUIDToString(workspaceID)
	return fmt.Sprintf("workspaces/%s/lark/%s%s", wsHex, id, ext)
}

// pgUUIDToString stringifies a pgtype.UUID using the standard
// 8-4-4-4-12 dashed format. Kept local so we don't import util/.
func pgUUIDToString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// larkAttachmentExt picks a file extension for the storage key. Prefer
// the user-supplied filename's extension (already correct most of
// the time), then fall back to a mime-derived suffix.
func larkAttachmentExt(att LarkMessageAttachment, displayName string) string {
	if displayName != "" {
		if ext := path.Ext(displayName); ext != "" {
			return ext
		}
	}
	switch att.MimeType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	}
	if att.ResourceType == "image" {
		return ".bin"
	}
	return ""
}

// larkSyntheticAttachmentName builds a stable display name for image
// messages (which don't carry a user-supplied filename). The format
// is `image-<file_key_suffix>.bin` so the displayed name still makes
// it obvious which Lark resource it came from.
func larkSyntheticAttachmentName(att LarkMessageAttachment) string {
	suffix := att.FileKey
	if len(suffix) > 16 {
		suffix = suffix[len(suffix)-16:]
	}
	if att.ResourceType == "image" {
		return fmt.Sprintf("lark-image-%s", suffix)
	}
	return fmt.Sprintf("lark-file-%s", suffix)
}

// pickMime returns the most authoritative mime hint we have. Lark's
// list endpoint hints at file MimeType but not at all on image
// messages, and the resource download's Content-Type is authoritative
// for the actual bytes — that wins when present.
func pickMime(envelope, contentType string) string {
	if contentType != "" {
		// Lark sometimes returns Content-Type with a charset (e.g.
		// "image/png; charset=utf-8" — harmless mistake on their end).
		// Trim that off so the mime whitelist match works.
		if i := strings.Index(contentType, ";"); i >= 0 {
			return strings.TrimSpace(contentType[:i])
		}
		return strings.TrimSpace(contentType)
	}
	return strings.TrimSpace(envelope)
}

// formatByteSize renders an int64 as a short human-readable size
// suitable for inline notices (`12.3 MB`). Used in the
// "[oversized attachment: <name> <size>]" placeholder.
func formatByteSize(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
	)
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Compile-time guard: the LarkResourceError type satisfies the error
// interface. Useful because callers often `errors.As` it.
var _ error = (*LarkResourceError)(nil)
