import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api";
import { runtimeKeys } from "../runtimes/queries";
import { computerKeys } from "./queries";

// useDeleteComputer wraps `DELETE /api/computers/:id` (RFC v6.1 §6.3). The
// server refuses with 409 when any runtime under the computer is still in
// use; the body carries id lists for active agents and active tasks (D2)
// so the UI can route the user to "See agents" / "See tasks" instead of
// fronting a faceless toast.
//
// We also invalidate runtimeKeys because every runtime hosted by this
// daemon is implicitly gone — the runtime list page should reflect that
// without a separate refetch.
export function useDeleteComputer(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (computerId: string) => api.deleteComputer(computerId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: computerKeys.all(wsId) });
      qc.invalidateQueries({ queryKey: runtimeKeys.all(wsId) });
    },
  });
}

// ComputerInUse is the parsed shape of the 409 body returned by
// DELETE /api/computers/:id when the computer still has occupants. Both
// arrays may be empty; at least one is non-empty when the server returns
// 409. The legacy `active_agents` numeric is no longer emitted by the
// server (D2 / RFC v6.1 §6.3) — surface counts as `agents.length`.
export type ComputerInUse = {
  agents: string[];
  tasks: string[];
};

/**
 * Parses the 409 response body for `DELETE /api/computers/:id` (D2). Returns
 * null when the error is not a 409 or doesn't carry the expected fields, so
 * callers can fall through to their generic error toast.
 */
export function computerInUseFromError(err: unknown): ComputerInUse | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const body = err.body as Record<string, unknown> | null | undefined;
  if (!body) return null;
  const agents = readIdArray(body.active_agents);
  const tasks = readIdArray(body.active_tasks);
  if (agents.length === 0 && tasks.length === 0) return null;
  return { agents, tasks };
}

function readIdArray(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  const out: string[] = [];
  for (const item of v) {
    if (typeof item === "string" && item) out.push(item);
  }
  return out;
}
