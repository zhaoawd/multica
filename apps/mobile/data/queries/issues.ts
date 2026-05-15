/**
 * Issue queries — workspace-wide list, single-issue detail, timeline.
 * Mobile-owned; mirrors a strict subset of packages/core/issues/queries.ts.
 *
 * Query keys live in ./issue-keys so detail / timeline / list / myList all
 * sit under the `issues/<wsId>` prefix — WS handlers can invalidate the
 * whole subtree with one call when needed.
 */
import {
  infiniteQueryOptions,
  queryOptions,
} from "@tanstack/react-query";
import type { TimelinePage } from "@multica/core/types";
import { api } from "@/data/api";
import { issueKeys } from "./issue-keys";

type TimelineCursor = { mode: "before"; cursor: string } | null;

export { issueKeys } from "./issue-keys";

/**
 * Workspace-wide issue list. Backend filters by `X-Workspace-Slug` header
 * (root CLAUDE.md "All queries filter by workspace_id"), so we pass an
 * empty params object — server returns every issue the user is allowed to
 * see in the current workspace.
 *
 * Cache shape: flat `Issue[]` (we strip `.issues` from the response) so
 * the WS updaters can patch this list with the same shape as
 * myIssueListOptions. Pagination is deferred — web's `IssuesPage` also
 * fetches all in one shot today (`packages/views/issues/components/
 * issues-page.tsx:30`).
 */
export const issueListOptions = (wsId: string | null) =>
  queryOptions({
    queryKey: issueKeys.list(wsId),
    queryFn: async ({ signal }) => {
      const res = await api.listIssues({}, { signal });
      return res.issues;
    },
    enabled: !!wsId,
  });

export const issueDetailOptions = (wsId: string | null, id: string) =>
  queryOptions({
    queryKey: issueKeys.detail(wsId, id),
    queryFn: ({ signal }) => api.getIssue(id, { signal }),
    enabled: !!wsId && !!id,
  });

export const issueTimelineInfiniteOptions = (
  wsId: string | null,
  id: string,
) =>
  infiniteQueryOptions<
    TimelinePage,
    Error,
    { pages: TimelinePage[]; pageParams: TimelineCursor[] },
    readonly unknown[],
    TimelineCursor
  >({
    queryKey: issueKeys.timeline(wsId, id),
    queryFn: ({ pageParam, signal }) =>
      api.listTimeline(id, pageParam, undefined, { signal }),
    initialPageParam: null,
    getNextPageParam: (lastPage) =>
      lastPage.has_more_before && lastPage.next_cursor
        ? { mode: "before" as const, cursor: lastPage.next_cursor }
        : undefined,
    enabled: !!wsId && !!id,
  });
