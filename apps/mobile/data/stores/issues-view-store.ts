/**
 * View store for the workspace-wide Issues page (`more/issues.tsx`).
 * Same shape as `useMyIssuesViewStore` minus the `scope` field —
 * workspace Issues has no Assigned/Created/Agents tabs (every issue is in
 * scope by definition).
 *
 * Why a separate store and not a shared one: a user filtering Done out of
 * "My Issues" shouldn't have that filter spill into the workspace Issues
 * page (and vice versa). They're conceptually different surfaces with
 * independent filter intent.
 *
 * Empty filter array = "show all" (matches web's predicate semantics in
 * packages/views/issues/utils/filter.ts).
 *
 * No persist middleware — matches the existing mobile pattern (filters
 * are session-scoped; auth/workspace are the only persisted stores).
 */
import { create } from "zustand";
import type { IssuePriority, IssueStatus } from "@multica/core/types";

interface IssuesViewState {
  statusFilters: IssueStatus[];
  priorityFilters: IssuePriority[];
  toggleStatusFilter: (status: IssueStatus) => void;
  togglePriorityFilter: (priority: IssuePriority) => void;
  clearFilters: () => void;
}

export const useIssuesViewStore = create<IssuesViewState>((set) => ({
  statusFilters: [],
  priorityFilters: [],
  toggleStatusFilter: (status) =>
    set((state) => ({
      statusFilters: state.statusFilters.includes(status)
        ? state.statusFilters.filter((s) => s !== status)
        : [...state.statusFilters, status],
    })),
  togglePriorityFilter: (priority) =>
    set((state) => ({
      priorityFilters: state.priorityFilters.includes(priority)
        ? state.priorityFilters.filter((p) => p !== priority)
        : [...state.priorityFilters, priority],
    })),
  clearFilters: () => set({ statusFilters: [], priorityFilters: [] }),
}));
