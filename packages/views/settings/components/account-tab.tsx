"use client";

import { useEffect, useRef, useState } from "react";
import { Camera, Loader2, Save } from "lucide-react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { Button } from "@multica/ui/components/ui/button";
import { Card, CardContent } from "@multica/ui/components/ui/card";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import { api } from "@multica/core/api";
import { useFileUpload } from "@multica/core/hooks/use-file-upload";
import { larkKeys, larkUserLinkOptions } from "@multica/core/lark/queries";
import { useT } from "../../i18n";

// Lark brand mark — kept in sync with the variant in integrations-tab.tsx.
// Lucide has no Lark/Feishu icon; we ship a minimal inline stand-in
// (rounded square + speech-bubble dots) that reads as a chat integration.
function LarkMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={className} fill="currentColor">
      <path d="M4 6.5A2.5 2.5 0 0 1 6.5 4h11A2.5 2.5 0 0 1 20 6.5v8A2.5 2.5 0 0 1 17.5 17H10l-4 3v-3.05a2.5 2.5 0 0 1-2-2.45v-8Zm5 3a1 1 0 1 0 0 2 1 1 0 0 0 0-2Zm4 0a1 1 0 1 0 0 2 1 1 0 0 0 0-2Zm4 0a1 1 0 1 0 0 2 1 1 0 0 0 0-2Z" />
    </svg>
  );
}

export function AccountTab() {
  const { t } = useT("settings");
  const user = useAuthStore((s) => s.user);
  const setUser = useAuthStore((s) => s.setUser);

  const [profileName, setProfileName] = useState(user?.name ?? "");
  const [profileSaving, setProfileSaving] = useState(false);
  const { upload, uploading } = useFileUpload(api);
  const fileInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    setProfileName(user?.name ?? "");
  }, [user]);

  const initials = (user?.name ?? "")
    .split(" ")
    .map((w) => w[0])
    .join("")
    .toUpperCase()
    .slice(0, 2);

  const handleAvatarUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    // Reset input so the same file can be re-selected
    e.target.value = "";
    try {
      const result = await upload(file);
      if (!result) return;
      const updated = await api.updateMe({ avatar_url: result.link });
      setUser(updated);
      toast.success(t(($) => $.account.toast_avatar_updated));
    } catch (err) {
      toast.error(err instanceof Error ? err.message : t(($) => $.account.toast_avatar_failed));
    }
  };

  const handleProfileSave = async () => {
    setProfileSaving(true);
    try {
      const updated = await api.updateMe({ name: profileName });
      setUser(updated);
      toast.success(t(($) => $.account.toast_profile_updated));
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.account.toast_profile_failed));
    } finally {
      setProfileSaving(false);
    }
  };

  return (
    <div className="space-y-8">
      <section className="space-y-4">
        <h2 className="text-sm font-semibold">{t(($) => $.account.section_profile)}</h2>

        <Card>
          <CardContent className="space-y-4">
            {/* Avatar upload */}
            <div className="flex items-center gap-4">
              <button
                type="button"
                className="group relative h-16 w-16 shrink-0 rounded-full bg-muted overflow-hidden focus:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                onClick={() => fileInputRef.current?.click()}
                disabled={uploading}
              >
                {user?.avatar_url ? (
                  <img
                    src={user.avatar_url}
                    alt={user.name}
                    className="h-full w-full object-cover"
                  />
                ) : (
                  <span className="flex h-full w-full items-center justify-center text-lg font-semibold text-muted-foreground">
                    {initials}
                  </span>
                )}
                <div className="absolute inset-0 flex items-center justify-center bg-black/40 opacity-0 transition-opacity group-hover:opacity-100">
                  {uploading ? (
                    <Loader2 className="h-5 w-5 animate-spin text-white" />
                  ) : (
                    <Camera className="h-5 w-5 text-white" />
                  )}
                </div>
              </button>
              <input
                ref={fileInputRef}
                type="file"
                accept="image/*"
                className="hidden"
                onChange={handleAvatarUpload}
              />
              <div className="text-xs text-muted-foreground">
                {t(($) => $.account.click_avatar_hint)}
              </div>
            </div>

            <div>
              <Label className="text-xs text-muted-foreground">{t(($) => $.account.name_label)}</Label>
              <Input
                type="search"
                value={profileName}
                onChange={(e) => setProfileName(e.target.value)}
                className="mt-1"
              />
            </div>
            <div className="flex items-center justify-end gap-2 pt-1">
              <Button
                size="sm"
                onClick={handleProfileSave}
                disabled={profileSaving || !profileName.trim()}
              >
                <Save className="h-3 w-3" />
                {profileSaving ? t(($) => $.account.saving) : t(($) => $.account.save)}
              </Button>
            </div>
          </CardContent>
        </Card>
      </section>

      <LinkedAccountsSection />
    </div>
  );
}

// ── Linked accounts ────────────────────────────────────────────────────────
//
// Per-user external account links. Only Lark today (Phase 2.1). When more
// providers land (GitHub user identity, Google, ...), add them as siblings
// under the same section rather than spinning up a new page.

function LinkedAccountsSection() {
  const { t } = useT("settings");
  const queryClient = useQueryClient();
  const { data: link } = useQuery(larkUserLinkOptions());

  // Surface the OAuth callback outcome. The backend redirects the browser
  // back to the page that initiated the connect (the path is sealed inside
  // the state HMAC) with `?lark_linked=1` or `?lark_error=<reason>`. We
  // pop the toast once and strip the params so a refresh doesn't re-fire.
  useEffect(() => {
    if (typeof window === "undefined") return;
    const params = new URLSearchParams(window.location.search);
    const linked = params.get("lark_linked");
    const error = params.get("lark_error");
    if (!linked && !error) return;

    if (linked) {
      toast.success(t(($) => $.account.lark_toast_connected));
      queryClient.invalidateQueries({ queryKey: larkKeys.userLink() });
    } else if (error) {
      // Explicit switch over known error codes — the backend's list is small
      // and stable, and an enum-style mapping keeps i18n key access typed.
      // Unknown codes fall through to the generic message.
      let msg: string;
      switch (error) {
        case "invalid_state":
          msg = t(($) => $.account.lark_toast_callback_error_invalid_state);
          break;
        case "not_configured":
          msg = t(($) => $.account.lark_toast_callback_error_not_configured);
          break;
        case "exchange_failed":
          msg = t(($) => $.account.lark_toast_callback_error_exchange_failed);
          break;
        case "already_linked_to_other_user":
          msg = t(($) => $.account.lark_toast_callback_error_already_linked_to_other_user);
          break;
        default:
          msg = t(($) => $.account.lark_toast_callback_error_generic);
      }
      toast.error(msg);
    }
    params.delete("lark_linked");
    params.delete("lark_error");
    const next = window.location.pathname + (params.toString() ? "?" + params.toString() : "");
    window.history.replaceState({}, "", next);
  }, [queryClient, t]);

  const startMutation = useMutation({
    mutationFn: async () => {
      const returnTo =
        typeof window !== "undefined" ? window.location.pathname + window.location.search : undefined;
      return api.startLarkUserLink(returnTo);
    },
    onSuccess: (res) => {
      if (!res.url) {
        toast.error(t(($) => $.account.lark_toast_start_failed));
        return;
      }
      // Top-level navigation; `.assign` (over `.href = `) is easier to spy
      // on in jsdom tests and behaves identically in browsers.
      window.location.assign(res.url);
    },
    onError: (e: unknown) => {
      toast.error(e instanceof Error ? e.message : t(($) => $.account.lark_toast_start_failed));
    },
  });

  const deleteMutation = useMutation({
    mutationFn: () => api.deleteMyLarkUserLink(),
    onSuccess: () => {
      toast.success(t(($) => $.account.lark_toast_disconnected));
      queryClient.invalidateQueries({ queryKey: larkKeys.userLink() });
    },
    onError: (e: unknown) => {
      toast.error(e instanceof Error ? e.message : t(($) => $.account.lark_toast_disconnect_failed));
    },
  });

  const configured = link?.configured ?? false;
  const linked = link?.linked ?? false;
  const openId = link?.open_id;

  return (
    <section className="space-y-4">
      <h2 className="text-sm font-semibold">{t(($) => $.account.section_linked_accounts)}</h2>
      <p className="text-xs text-muted-foreground">
        {t(($) => $.account.linked_accounts_description)}
      </p>

      <Card>
        <CardContent className="space-y-4">
          <div className="flex items-start justify-between gap-4">
            <div className="flex items-start gap-3 min-w-0">
              <LarkMark className="h-6 w-6 mt-0.5 shrink-0" />
              <div className="space-y-1 min-w-0">
                <p className="text-sm font-medium">{t(($) => $.account.lark_title)}</p>
                <p className="text-xs text-muted-foreground">
                  {t(($) => $.account.lark_description)}
                </p>
                {!configured && (
                  <p className="text-xs text-amber-600 dark:text-amber-500">
                    {t(($) => $.account.lark_not_configured)}
                  </p>
                )}
                {configured && linked && openId && (
                  <p className="text-xs text-muted-foreground truncate">
                    {t(($) => $.account.lark_status_linked_as)}{" "}
                    <code className="rounded bg-muted px-1 py-0.5 text-[10px]">{openId}</code>
                  </p>
                )}
                {configured && !linked && (
                  <p className="text-xs text-muted-foreground">
                    {t(($) => $.account.lark_status_not_linked)}
                  </p>
                )}
              </div>
            </div>

            <div className="shrink-0">
              {linked ? (
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => deleteMutation.mutate()}
                  disabled={deleteMutation.isPending}
                >
                  {deleteMutation.isPending
                    ? t(($) => $.account.lark_disconnecting)
                    : t(($) => $.account.lark_disconnect)}
                </Button>
              ) : (
                <Button
                  size="sm"
                  onClick={() => startMutation.mutate()}
                  disabled={!configured || startMutation.isPending}
                >
                  {startMutation.isPending
                    ? t(($) => $.account.lark_connecting)
                    : t(($) => $.account.lark_connect)}
                </Button>
              )}
            </div>
          </div>
        </CardContent>
      </Card>
    </section>
  );
}
