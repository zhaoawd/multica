"use client";

import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { larkBindingOptions, larkKeys } from "@multica/core/lark/queries";
import type { LarkBindingResponse } from "@multica/core/types";
import { api } from "@multica/core/api";
import { useT } from "../../i18n";

function LarkMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={className} fill="currentColor">
      <path d="M4 6.5A2.5 2.5 0 0 1 6.5 4h11A2.5 2.5 0 0 1 20 6.5v8A2.5 2.5 0 0 1 17.5 17H10l-4 3v-3.05a2.5 2.5 0 0 1-2-2.45v-8Zm5 3a1 1 0 1 0 0 2 1 1 0 0 0 0-2Zm4 0a1 1 0 1 0 0 2 1 1 0 0 0 0-2Zm4 0a1 1 0 1 0 0 2 1 1 0 0 0 0-2Z" />
    </svg>
  );
}

// GitHub now lives in its own Settings tab (see github-tab.tsx). This tab
// hosts non-GitHub third-party integrations — currently only Lark.
export function IntegrationsTab() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);
  const { data: members = [] } = useQuery(memberListOptions(wsId));

  const currentMember = members.find((m) => m.user_id === user?.id) ?? null;
  const canManage = currentMember?.role === "owner" || currentMember?.role === "admin";

  return (
    <div className="space-y-4">
      <section className="space-y-4">
        <h2 className="text-sm font-semibold">{t(($) => $.integrations.section_title)}</h2>
        <LarkIntegrationCard wsId={wsId} canManage={canManage} />
      </section>
    </div>
  );
}

// LarkIntegrationCard renders the chat binding + event allowlist UI.
// We keep it inline in this file so the Settings → Integrations page stays
// a single import surface; once Lark grows beyond binding (P2 OAuth, P3
// docs) it will graduate into its own file under settings/lark/.
function LarkIntegrationCard({ wsId, canManage }: { wsId: string; canManage: boolean }) {
  const { t } = useT("settings");
  const qc = useQueryClient();
  const enabled = !!wsId && canManage;

  const { data: binding } = useQuery({ ...larkBindingOptions(wsId), enabled });

  const [chatID, setChatID] = useState("");
  const [eventSet, setEventSet] = useState<Set<string>>(new Set());

  // Sync local form state when the server response arrives or changes —
  // we don't want stale input lingering after a save / reconnect.
  useEffect(() => {
    if (!binding) return;
    setChatID(binding.chat_id ?? "");
    setEventSet(new Set(binding.enabled_events));
  }, [binding]);

  const supportedEvents = useMemo(
    () => binding?.supported_events ?? defaultSupportedEvents,
    [binding?.supported_events],
  );
  const configured = binding?.configured ?? false;
  const bound = binding?.bound ?? false;

  const upsert = useMutation({
    mutationFn: () =>
      api.upsertLarkBinding(wsId, {
        chat_id: chatID.trim(),
        enabled_events: Array.from(eventSet),
      }),
    onSuccess: (resp) => {
      qc.setQueryData<LarkBindingResponse>(larkKeys.binding(wsId), resp);
      toast.success(t(($) => $.integrations.lark_toast_saved));
    },
    onError: (e) => {
      toast.error(e instanceof Error ? e.message : t(($) => $.integrations.lark_toast_save_failed));
    },
  });

  const disconnect = useMutation({
    mutationFn: () => api.deleteLarkBinding(wsId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: larkKeys.binding(wsId) });
      setChatID("");
      setEventSet(new Set());
      toast.success(t(($) => $.integrations.lark_toast_disconnected));
    },
    onError: (e) => {
      toast.error(e instanceof Error ? e.message : t(($) => $.integrations.lark_toast_save_failed));
    },
  });

  function toggleEvent(key: string) {
    setEventSet((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  return (
    <Card>
      <CardContent className="space-y-4">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-start gap-3">
            <LarkMark className="h-6 w-6 mt-0.5 shrink-0 text-foreground/80" />
            <div className="space-y-1">
              <p className="text-sm font-medium">{t(($) => $.integrations.lark_title)}</p>
              <p className="text-xs text-muted-foreground">
                {t(($) => $.integrations.lark_description)}
              </p>
            </div>
          </div>
          {canManage && bound && configured && (
            <Button
              size="sm"
              variant="outline"
              onClick={() => disconnect.mutate()}
              disabled={disconnect.isPending}
            >
              {disconnect.isPending
                ? t(($) => $.integrations.lark_disconnecting)
                : t(($) => $.integrations.lark_disconnect)}
            </Button>
          )}
        </div>

        {canManage && !configured && (
          <p className="text-xs text-muted-foreground">
            {t(($) => $.integrations.lark_not_configured)}{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-[10px]">LARK_APP_ID</code>,{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-[10px]">LARK_APP_SECRET</code>,{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-[10px]">LARK_VERIFICATION_TOKEN</code>,{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-[10px]">LARK_ENCRYPT_KEY</code>.
          </p>
        )}

        {canManage && configured && (
          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="lark-chat-id" className="text-xs">
                {t(($) => $.integrations.lark_chat_id_label)}
              </Label>
              <Input
                id="lark-chat-id"
                placeholder={t(($) => $.integrations.lark_chat_id_placeholder)}
                value={chatID}
                onChange={(e) => setChatID(e.target.value)}
              />
              <p className="text-[11px] text-muted-foreground">
                {t(($) => $.integrations.lark_chat_id_hint)}
              </p>
            </div>

            <div className="space-y-2">
              <p className="text-xs font-medium">
                {t(($) => $.integrations.lark_events_label)}
              </p>
              <ul className="space-y-1.5">
                {supportedEvents.map((key) => {
                  // Inline switch so `t` keeps its namespace narrowing —
                  // extracting this into a helper that takes `t` as a
                  // parameter widens the type to all namespaces and
                  // breaks the `$ => $.integrations.*` path.
                  let label = key;
                  switch (key) {
                    case "issue:created":
                      label = t(($) => $.integrations.lark_event_issue_created);
                      break;
                    case "issue:updated":
                      label = t(($) => $.integrations.lark_event_issue_updated);
                      break;
                    case "task:completed":
                      label = t(($) => $.integrations.lark_event_task_completed);
                      break;
                    case "task:failed":
                      label = t(($) => $.integrations.lark_event_task_failed);
                      break;
                    case "comment:created":
                      label = t(($) => $.integrations.lark_event_comment_created);
                      break;
                  }
                  return (
                    <li key={key}>
                      <label className="flex cursor-pointer items-center gap-2 text-xs">
                        <Checkbox
                          checked={eventSet.has(key)}
                          onCheckedChange={() => toggleEvent(key)}
                        />
                        <span className="font-medium">{label}</span>
                        <code className="rounded bg-muted px-1 py-0.5 text-[10px] text-muted-foreground">
                          {key}
                        </code>
                      </label>
                    </li>
                  );
                })}
              </ul>
            </div>

            <div className="flex justify-end">
              <Button
                size="sm"
                onClick={() => upsert.mutate()}
                disabled={upsert.isPending || !chatID.trim()}
              >
                {upsert.isPending
                  ? t(($) => $.integrations.lark_saving)
                  : bound
                  ? t(($) => $.integrations.lark_save)
                  : t(($) => $.integrations.lark_connect)}
              </Button>
            </div>
          </div>
        )}

        {!canManage && (
          <p className="text-xs text-muted-foreground">
            {t(($) => $.integrations.manage_hint)}
          </p>
        )}
      </CardContent>
    </Card>
  );
}

// defaultSupportedEvents lets the UI render the checklist even before the
// first /lark/binding response lands. The server is still authoritative —
// once `binding.supported_events` arrives we replace this list — but the
// initial paint shouldn't be empty.
const defaultSupportedEvents = [
  "issue:created",
  "issue:updated",
  "task:completed",
  "task:failed",
  "comment:created",
];
