/**
 * Workspace-wide Issues page. Mirrors the data model of web's
 * `packages/views/issues/components/issues-page.tsx`: fetch every issue in
 * the workspace, group by status, expose status + priority filters.
 *
 * Differences vs My Issues (`(tabs)/my-issues.tsx`):
 *   - No scope tabs (Assigned/Created/Agents) — workspace-wide list has
 *     no per-user scope to switch between.
 *   - Fetches `issueListOptions(wsId)` (workspace-wide) instead of
 *     `myIssueListOptions` (user-scoped).
 *   - Independent filter store (`useIssuesViewStore`) so workspace-level
 *     filters don't bleed into the per-user view.
 *
 * Everything else (SectionList grouped by status, IssueRow, IssueFilterSheet,
 * pull-to-refresh, empty state) is the same component family used across
 * the issue surfaces — keeps visual identity consistent (apps/mobile/
 * CLAUDE.md "Visual alignment is baseline").
 *
 * Filters beyond status/priority (assignee / project / label / creator)
 * are deferred — see plan; v1 ships with the same filter set as My Issues
 * for consistency and code reuse.
 */
import { useEffect, useMemo, useRef, useState } from "react";
import {
  ActivityIndicator,
  Pressable,
  SectionList,
  View,
} from "react-native";
import { SafeAreaView } from "react-native-safe-area-context";
import { useQuery } from "@tanstack/react-query";
import { router } from "expo-router";
import Svg, { Line } from "react-native-svg";
import type { Issue, IssuePriority, IssueStatus } from "@multica/core/types";
import { Text } from "@/components/ui/text";
import { Button } from "@/components/ui/button";
import { ScreenHeader } from "@/components/ui/screen-header";
import { StatusIcon } from "@/components/ui/status-icon";
import { IssueRow } from "@/components/issue/issue-row";
import { IssueFilterSheet } from "@/components/issue/issue-filter-sheet";
import { issueListOptions } from "@/data/queries/issues";
import { useWorkspaceStore } from "@/data/workspace-store";
import { useIssuesViewStore } from "@/data/stores/issues-view-store";
import {
  BOARD_STATUSES,
  PRIORITY_LABEL,
  STATUS_LABEL,
} from "@/lib/issue-status";
import { filterIssues } from "@/lib/filter-issues";

type IssueSection = { status: IssueStatus; data: Issue[] };

export default function IssuesPage() {
  const wsId = useWorkspaceStore((s) => s.currentWorkspaceId);
  const wsSlug = useWorkspaceStore((s) => s.currentWorkspaceSlug);

  const statusFilters = useIssuesViewStore((s) => s.statusFilters);
  const priorityFilters = useIssuesViewStore((s) => s.priorityFilters);

  const [sheetOpen, setSheetOpen] = useState(false);

  // Mirror useClearFiltersOnWorkspaceChange (packages/core/issues/stores/
  // view-store.ts:273-284): clear filters on transitions between two
  // defined workspace ids. Ref guard skips first render so we don't wipe
  // initial state on mount.
  const prevWsRef = useRef<string | null>(null);
  useEffect(() => {
    if (prevWsRef.current && wsId && wsId !== prevWsRef.current) {
      useIssuesViewStore.getState().clearFilters();
    }
    prevWsRef.current = wsId ?? null;
  }, [wsId]);

  const { data, isLoading, error, refetch, isRefetching } = useQuery(
    issueListOptions(wsId),
  );

  // Apply client-side status + priority filter. Mirrors the predicate at
  // packages/views/issues/utils/filter.ts:30-34 via filterIssues().
  const filtered = useMemo(
    () => filterIssues(data ?? [], statusFilters, priorityFilters),
    [data, statusFilters, priorityFilters],
  );

  // When statusFilters is non-empty, intersect visible status order with it
  // so hidden statuses don't render an empty section header.
  const sections = useMemo<IssueSection[]>(() => {
    if (filtered.length === 0) return [];
    const byStatus = new Map<IssueStatus, Issue[]>();
    for (const issue of filtered) {
      const list = byStatus.get(issue.status);
      if (list) list.push(issue);
      else byStatus.set(issue.status, [issue]);
    }
    const visibleStatuses =
      statusFilters.length > 0
        ? BOARD_STATUSES.filter((s) => statusFilters.includes(s))
        : BOARD_STATUSES;
    return visibleStatuses
      .map((status) => ({ status, data: byStatus.get(status) ?? [] }))
      .filter((s) => s.data.length > 0);
  }, [filtered, statusFilters]);

  const hasActiveFilters =
    statusFilters.length > 0 || priorityFilters.length > 0;

  const showEmptyState =
    !isLoading && !error && filtered.length === 0;

  return (
    <SafeAreaView className="flex-1 bg-background" edges={["bottom"]}>
      <ScreenHeader
        title="Issues"
        right={
          <FilterButton
            hasActive={hasActiveFilters}
            onPress={() => setSheetOpen(true)}
          />
        }
      />
      {hasActiveFilters ? (
        <ActiveFilterChips
          statusFilters={statusFilters}
          priorityFilters={priorityFilters}
          onClearStatus={(s) =>
            useIssuesViewStore.getState().toggleStatusFilter(s)
          }
          onClearPriority={(p) =>
            useIssuesViewStore.getState().togglePriorityFilter(p)
          }
        />
      ) : null}
      {isLoading ? (
        <View className="flex-1 items-center justify-center">
          <ActivityIndicator />
        </View>
      ) : error ? (
        <View className="px-4 gap-3">
          <Text className="text-sm text-destructive">
            Failed to load issues:{" "}
            {error instanceof Error ? error.message : "unknown error"}
          </Text>
          <Button variant="outline" onPress={() => refetch()}>
            Retry
          </Button>
        </View>
      ) : showEmptyState ? (
        <EmptyState
          message={
            hasActiveFilters
              ? "No issues match the current filters."
              : "No issues in this workspace yet."
          }
        />
      ) : (
        <SectionList
          sections={sections}
          keyExtractor={(item) => item.id}
          stickySectionHeadersEnabled={false}
          ItemSeparatorComponent={() => (
            <View className="h-px bg-border ml-4" />
          )}
          renderSectionHeader={({ section }) => (
            <SectionHeader
              status={section.status}
              count={section.data.length}
            />
          )}
          contentContainerClassName="pb-6"
          renderItem={({ item }) => (
            <IssueRow
              issue={item}
              onPress={() => {
                if (wsSlug) router.push(`/${wsSlug}/issue/${item.id}`);
              }}
            />
          )}
          refreshing={isRefetching}
          onRefresh={refetch}
        />
      )}

      <IssueFilterSheet
        visible={sheetOpen}
        onClose={() => setSheetOpen(false)}
        statusFilters={statusFilters}
        priorityFilters={priorityFilters}
        onToggleStatus={(s) =>
          useIssuesViewStore.getState().toggleStatusFilter(s)
        }
        onTogglePriority={(p) =>
          useIssuesViewStore.getState().togglePriorityFilter(p)
        }
        onClearFilters={() => useIssuesViewStore.getState().clearFilters()}
      />
    </SafeAreaView>
  );
}

function ActiveFilterChips({
  statusFilters,
  priorityFilters,
  onClearStatus,
  onClearPriority,
}: {
  statusFilters: IssueStatus[];
  priorityFilters: IssuePriority[];
  onClearStatus: (s: IssueStatus) => void;
  onClearPriority: (p: IssuePriority) => void;
}) {
  return (
    <View className="flex-row flex-wrap gap-1.5 px-4 pb-2">
      {statusFilters.map((s) => (
        <Chip
          key={`s-${s}`}
          label={STATUS_LABEL[s]}
          onClear={() => onClearStatus(s)}
        />
      ))}
      {priorityFilters.map((p) => (
        <Chip
          key={`p-${p}`}
          label={PRIORITY_LABEL[p]}
          onClear={() => onClearPriority(p)}
        />
      ))}
    </View>
  );
}

function Chip({ label, onClear }: { label: string; onClear: () => void }) {
  return (
    <Pressable
      onPress={onClear}
      className="flex-row items-center gap-1 pl-2.5 pr-2 py-1 rounded-full border border-border bg-secondary/40 active:bg-secondary"
    >
      <Text className="text-xs text-foreground">{label}</Text>
      <Svg width={10} height={10} viewBox="0 0 10 10">
        <Line x1="2" y1="2" x2="8" y2="8" stroke="#71717a" strokeWidth="1.5" strokeLinecap="round" />
        <Line x1="8" y1="2" x2="2" y2="8" stroke="#71717a" strokeWidth="1.5" strokeLinecap="round" />
      </Svg>
    </Pressable>
  );
}

function SectionHeader({
  status,
  count,
}: {
  status: IssueStatus;
  count: number;
}) {
  return (
    <View className="flex-row items-center gap-2 px-4 py-2 bg-background">
      <StatusIcon status={status} size={14} />
      <Text className="text-xs uppercase tracking-wider text-muted-foreground font-medium">
        {STATUS_LABEL[status]}
      </Text>
      <Text className="text-xs text-muted-foreground/60">{count}</Text>
    </View>
  );
}

function EmptyState({ message }: { message: string }) {
  return (
    <View className="flex-1 items-center justify-center px-6">
      <Text className="text-sm text-muted-foreground text-center">
        {message}
      </Text>
    </View>
  );
}

function FilterButton({
  hasActive,
  onPress,
}: {
  hasActive: boolean;
  onPress: () => void;
}) {
  return (
    <Pressable
      onPress={onPress}
      className="size-9 items-center justify-center rounded-md border border-border bg-background active:bg-secondary"
    >
      <Svg width={16} height={16} viewBox="0 0 16 16">
        {/* Mirrors muted-foreground (#71717a) — same hex used by status-icon */}
        <Line x1="2" y1="4" x2="14" y2="4" stroke="#71717a" strokeWidth="1.5" strokeLinecap="round" />
        <Line x1="4" y1="8" x2="12" y2="8" stroke="#71717a" strokeWidth="1.5" strokeLinecap="round" />
        <Line x1="6" y1="12" x2="10" y2="12" stroke="#71717a" strokeWidth="1.5" strokeLinecap="round" />
      </Svg>
      {hasActive ? (
        <View className="absolute top-1 right-1 size-1.5 rounded-full bg-brand" />
      ) : null}
    </Pressable>
  );
}
