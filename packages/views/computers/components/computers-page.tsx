"use client";

import { useMemo, useState } from "react";
import { Monitor, Plus, Search } from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { computerListOptions } from "@multica/core/computers";
import { useWorkspacePaths } from "@multica/core/paths";
import type { Computer } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { AppLink, useNavigation } from "../../navigation";
import { PageHeader } from "../../layout/page-header";
import { useT } from "../../i18n";
import { AddComputerDialog } from "./add-computer-dialog";

// RFC v6.1 / §1.2-1.3: Computers index page. Owns the Add Computer modal
// state internally so both apps (web + desktop) get the same flow without
// needing per-platform wiring. The modal mints an install token, shows the
// one-liner, and detects new daemon registration via polling + the
// platform-wide daemon:* WS invalidation.
export function ComputersPage() {
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const navigation = useNavigation();
  const { t } = useT("computers");
  const [search, setSearch] = useState("");
  const [addOpen, setAddOpen] = useState(false);

  // `wsId` is guaranteed by WorkspaceRouteLayout for any route nested under
  // /:slug. We still guard with `enabled` so an in-flight workspace switch
  // doesn't fire a request keyed on the empty string.
  const { data: computers = [], isLoading } = useQuery({
    ...computerListOptions(wsId ?? ""),
    enabled: !!wsId,
  });

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return computers;
    return computers.filter((c) =>
      [c.name, c.device_info, c.kind, c.status]
        .map((x) => (x ?? "").toLowerCase())
        .some((s) => s.includes(q)),
    );
  }, [computers, search]);

  // Snapshot the computer IDs that exist the moment the user opens the
  // dialog. The install step uses this to detect "a *new* computer
  // appeared" rather than just "a computer is in the list" — otherwise the
  // step would auto-close on open whenever the workspace already has
  // computers.
  const [knownIds, setKnownIds] = useState<Set<string>>(new Set());
  const openAdd = () => {
    setKnownIds(new Set(computers.map((c) => c.id)));
    setAddOpen(true);
  };

  return (
    <div className="flex h-full min-h-0 flex-1 flex-col">
      <PageHeader className="justify-between px-5">
        <div className="flex items-center gap-3">
          <Monitor className="size-4 text-muted-foreground" />
          <h1 className="text-base font-medium">{t(($) => $.page.title)}</h1>
        </div>
        <div className="flex items-center gap-2">
          <div className="relative">
            <Search className="pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder={t(($) => $.page.search_placeholder)}
              className="h-8 w-56 pl-7 text-sm"
            />
          </div>
          <Button size="sm" onClick={openAdd}>
            <Plus className="size-3.5" />
            {t(($) => $.page.add_computer)}
          </Button>
        </div>
      </PageHeader>

      <div className="flex-1 overflow-y-auto px-5 py-4">
        {isLoading ? (
          <div className="space-y-2">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : filtered.length === 0 ? (
          <EmptyState query={search} onAddComputer={openAdd} />
        ) : (
          <ComputersTable rows={filtered} hrefFor={(id) => paths.computerDetail(id)} />
        )}
      </div>

      <AddComputerDialog
        open={addOpen}
        onOpenChange={setAddOpen}
        knownComputerIds={knownIds}
        onConnected={(id) => {
          navigation.push(paths.computerDetail(id));
        }}
      />
    </div>
  );
}

function EmptyState({ query, onAddComputer }: { query: string; onAddComputer?: () => void }) {
  const { t } = useT("computers");
  if (query.trim()) {
    return (
      <div className="rounded-md border border-dashed p-10 text-center text-sm text-muted-foreground">
        {t(($) => $.page.no_matches, { query })}
      </div>
    );
  }
  return (
    <div className="rounded-md border border-dashed p-10 text-center">
      <Monitor className="mx-auto mb-3 size-8 text-muted-foreground" />
      <div className="text-sm font-medium">{t(($) => $.page.empty.title)}</div>
      <p className="mx-auto mt-1 max-w-md text-sm text-muted-foreground">
        {t(($) => $.page.empty.hint)}
      </p>
      <Button size="sm" className="mt-4" onClick={onAddComputer}>
        <Plus className="size-3.5" />
        {t(($) => $.page.empty.cta)}
      </Button>
    </div>
  );
}

function ComputersTable({
  rows,
  hrefFor,
}: {
  rows: Computer[];
  hrefFor: (id: string) => string;
}) {
  const { t } = useT("computers");
  return (
    <div className="overflow-x-auto rounded-md border">
      <table className="w-full text-sm">
        <thead className="bg-muted/50 text-left text-xs uppercase tracking-wide text-muted-foreground">
          <tr>
            <th className="px-3 py-2 font-medium">{t(($) => $.list.col_name)}</th>
            <th className="px-3 py-2 font-medium">{t(($) => $.list.col_kind)}</th>
            <th className="px-3 py-2 font-medium">{t(($) => $.list.col_status)}</th>
            <th className="px-3 py-2 font-medium">{t(($) => $.list.col_runtimes)}</th>
            <th className="px-3 py-2 font-medium">{t(($) => $.list.col_last_seen)}</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((c) => (
            <tr key={c.id} className="border-t hover:bg-muted/30">
              <td className="px-3 py-2 font-medium">
                <AppLink href={hrefFor(c.id)} className="hover:underline">
                  {c.name || "(unnamed)"}
                </AppLink>
              </td>
              <td className="px-3 py-2 text-muted-foreground">{computerKindLabel(c.kind, t)}</td>
              <td className="px-3 py-2">
                <StatusDot status={c.status} t={t} />
              </td>
              <td className="px-3 py-2 text-muted-foreground">{c.runtime_count ?? 0}</td>
              <td className="px-3 py-2 text-muted-foreground">{formatLastSeen(c.last_seen_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function StatusDot({
  status,
  t,
}: {
  status: Computer["status"];
  t: ReturnType<typeof useT<"computers">>["t"];
}) {
  const isOnline = status === "online";
  return (
    <span className="inline-flex items-center gap-2 text-xs">
      <span
        className={`inline-block size-1.5 rounded-full ${
          isOnline ? "bg-success" : "bg-muted-foreground/40"
        }`}
      />
      {isOnline ? t(($) => $.list.status.online) : t(($) => $.list.status.offline)}
    </span>
  );
}

function computerKindLabel(
  kind: Computer["kind"],
  t: ReturnType<typeof useT<"computers">>["t"],
): string {
  // Defensive default for forward-compat: a future backend kind we don't
  // know yet renders as "unknown" rather than crashing.
  switch (kind) {
    case "desktop":
      return t(($) => $.list.kind.desktop);
    case "remote":
      return t(($) => $.list.kind.remote);
    case "cloud":
      return t(($) => $.list.kind.cloud);
    default:
      return t(($) => $.list.kind.unknown);
  }
}

function formatLastSeen(ts: string | null | undefined): string {
  if (!ts) return "—";
  try {
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return "—";
    return d.toLocaleString();
  } catch {
    return "—";
  }
}
