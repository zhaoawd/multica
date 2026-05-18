package handler

import (
	"context"
	"strings"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fetchLinkedDocsForIssue scans the issue description and recent comments
// for Lark doc URLs, fetches each one's plain-text content, and returns the
// LinkedDoc list that rides on the claim response (P3.A — see
// LARK_INTEGRATION_DESIGN.md §6.3).
//
// Returns nil when:
//   - The Lark integration is unconfigured (h.LarkDocs nil or env incomplete).
//   - The issue has no Lark URLs anywhere in its body or comments.
//
// Failures of individual fetches never bubble up — each one becomes a
// LinkedDoc{Error: ...} entry so the agent prompt renders a stable
// placeholder rather than silently dropping the reference.
//
// Bounding: at most service.MaxDocsPerClaim URLs are expanded per claim,
// with service.DocFetchTimeout per fetch. Comments are scanned up to
// service.CommentsScanLimit (covers the p99 issue easily). Worst-case
// added latency is MaxDocsPerClaim * DocFetchTimeout — the daemon's claim
// poll tolerates this because a doc-heavy task is by definition rare.
func (h *Handler) fetchLinkedDocsForIssue(ctx context.Context, issue db.Issue) []service.LinkedDoc {
	if h.LarkDocs == nil || !h.LarkDocs.Configured() {
		return nil
	}

	var sources []string
	if issue.Description.Valid && issue.Description.String != "" {
		sources = append(sources, issue.Description.String)
	}
	if comments, err := h.Queries.ListCommentsForIssue(ctx, db.ListCommentsForIssueParams{
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		Limit:       service.CommentsScanLimit,
	}); err == nil {
		for _, c := range comments {
			if c.Content != "" {
				sources = append(sources, c.Content)
			}
		}
	}
	if len(sources) == 0 {
		return nil
	}

	urls := service.ExtractDocURLs(strings.Join(sources, "\n"))
	if len(urls) == 0 {
		return nil
	}
	if len(urls) > service.MaxDocsPerClaim {
		urls = urls[:service.MaxDocsPerClaim]
	}

	out := make([]service.LinkedDoc, 0, len(urls))
	for _, u := range urls {
		fetchCtx, cancel := context.WithTimeout(ctx, service.DocFetchTimeout)
		out = append(out, h.LarkDocs.Fetch(fetchCtx, u))
		cancel()
	}
	return out
}
