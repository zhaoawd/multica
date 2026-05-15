/**
 * Workspace Issues realtime — listing-level subscription. Mounted globally
 * (workspace-session-lifetime) alongside `useMyIssuesRealtime` so the
 * workspace-wide list stays fresh regardless of which tab is foregrounded.
 *
 * issue:created     — payload includes the full Issue object; prepend in
 *                     place. Avoids a refetch and keeps UI snappy when
 *                     someone else creates an issue while we're looking
 *                     at the list.
 * issue:updated     — patch in-place; client-side filter/grouping
 *                     re-derives on the next render.
 * issue:deleted     — strip from the list.
 * issue_labels:changed — handled by `patchIssueLabels` in the shared
 *                     updaters (already patches `list(wsId)` too — see
 *                     `issue-ws-updaters.ts`). No subscription needed
 *                     here because `useMyIssuesRealtime` already drives
 *                     it for every workspace surface.
 * onReconnect       — invalidate `list(wsId)` since we may have missed
 *                     a create/delete while disconnected.
 *
 * This hook is independent of `useMyIssuesRealtime` (different cache key
 * `list(wsId)` vs `myAll(wsId)`). Both are listing-level and run in
 * parallel — apps/mobile/CLAUDE.md "Mobile-owned updaters" / "list-level
 * global, per-record per-screen".
 */
import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type {
  IssueCreatedPayload,
  IssueDeletedPayload,
  IssueUpdatedPayload,
} from "@multica/core/types";
import { issueKeys } from "@/data/queries/issue-keys";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useWSClient } from "./realtime-provider";
import {
  patchIssuesList,
  prependToIssuesList,
  removeFromIssuesList,
} from "./issue-ws-updaters";

export function useIssuesRealtime() {
  const ws = useWSClient();
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const qc = useQueryClient();

  useEffect(() => {
    if (!ws || !wsId) return;

    const invalidateList = () => {
      qc.invalidateQueries({ queryKey: issueKeys.list(wsId) });
    };

    const unsubs: Array<() => void> = [
      ws.on("issue:created", (p) => {
        const payload = p as IssueCreatedPayload;
        prependToIssuesList(qc, wsId, payload.issue);
      }),
      ws.on("issue:updated", (p) => {
        const payload = p as IssueUpdatedPayload;
        patchIssuesList(qc, wsId, payload.issue);
      }),
      ws.on("issue:deleted", (p) => {
        const payload = p as IssueDeletedPayload;
        removeFromIssuesList(qc, wsId, payload.issue_id);
      }),
      ws.onReconnect(invalidateList),
    ];

    return () => {
      for (const unsub of unsubs) unsub();
    };
  }, [ws, wsId, qc]);
}
