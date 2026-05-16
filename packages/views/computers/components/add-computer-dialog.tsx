"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowLeft,
  Check,
  Cloud,
  Copy,
  Download,
  Laptop,
  Loader2,
  Monitor,
  Server,
  Terminal,
} from "lucide-react";
import { toast } from "sonner";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { detectClientType } from "@multica/core/analytics";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspaceSlug } from "@multica/core/paths";
import { computerKeys, computerListOptions } from "@multica/core/computers";
import type { InstallTokenMintResponse } from "@multica/core/types";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Badge } from "@multica/ui/components/ui/badge";
import { useT } from "../../i18n";

type Step = "select" | "install";

export interface AddComputerDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** IDs already present when the dialog opened — used so the install step
   *  can detect *the* newly-registered computer rather than any existing one. */
  knownComputerIds: Set<string>;
  onConnected: (computerId: string) => void;
}

// RFC v6.1 / §6.4. Add Computer onboarding modal. Two steps:
//   select   — three cards: This Desktop / Another machine / Cloud (soon).
//   install  — mints a one-time install token, shows the one-liner curl
//              command, and polls computerListOptions until the new daemon
//              registers. WS daemon:* events already invalidate the list, so
//              polling is a safety net for environments where the WS is
//              flaky or the user has the page open through a sleep.
export function AddComputerDialog({
  open,
  onOpenChange,
  knownComputerIds,
  onConnected,
}: AddComputerDialogProps) {
  const [step, setStep] = useState<Step>("select");

  // Reset to step 1 every time the modal re-opens. Keeping local state
  // between opens would surface a stale install token from a prior session.
  useEffect(() => {
    if (open) setStep("select");
  }, [open]);

  // RFC v6.1 §1.3: Web swaps the "This Desktop" card for a "Only available in
  // Desktop" info card. We can't ship a daemon inside the browser tab, so the
  // Web variant just nudges the user toward the Desktop download instead of
  // pretending the option is real and silently dropping the click.
  const platform = detectClientType(); // "web" | "desktop"

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[85vh] flex-col sm:max-w-xl">
        {step === "select" ? (
          <SelectStep
            platform={platform}
            onPickThisDesktop={() => {
              onOpenChange(false);
            }}
            onPickAnother={() => setStep("install")}
            onClose={() => onOpenChange(false)}
          />
        ) : (
          <InstallStep
            knownComputerIds={knownComputerIds}
            onBack={() => setStep("select")}
            onConnected={(id) => {
              onOpenChange(false);
              onConnected(id);
            }}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

// ---------------------------------------------------------------------------
// Step 1: choose where the computer lives
// ---------------------------------------------------------------------------

function SelectStep({
  platform,
  onPickThisDesktop,
  onPickAnother,
  onClose,
}: {
  platform: "web" | "desktop";
  onPickThisDesktop: () => void;
  onPickAnother: () => void;
  onClose: () => void;
}) {
  const { t } = useT("computers");
  return (
    <>
      <DialogHeader>
        <DialogTitle>{t(($) => $.add.title)}</DialogTitle>
        <DialogDescription>{t(($) => $.add.subtitle)}</DialogDescription>
      </DialogHeader>

      <div className="flex flex-col gap-2">
        {platform === "desktop" ? (
          <Choice
            icon={<Laptop className="h-4 w-4" />}
            title={t(($) => $.add.this_desktop.title)}
            badge={t(($) => $.add.this_desktop.recommended)}
            description={t(($) => $.add.this_desktop.description)}
            cta={t(($) => $.add.this_desktop.cta)}
            onClick={onPickThisDesktop}
          />
        ) : (
          <Choice
            icon={<Laptop className="h-4 w-4" />}
            title={t(($) => $.add.this_desktop.title)}
            description={t(($) => $.add.this_desktop.web_only)}
            cta={t(($) => $.add.this_desktop.download_cta)}
            href="https://multica.ai/download"
            external
            ctaIcon={<Download className="h-3 w-3" />}
          />
        )}
        <Choice
          icon={<Server className="h-4 w-4" />}
          title={t(($) => $.add.another_machine.title)}
          description={t(($) => $.add.another_machine.description)}
          cta={t(($) => $.add.another_machine.cta)}
          onClick={onPickAnother}
        />
        <Choice
          icon={<Cloud className="h-4 w-4" />}
          title={t(($) => $.add.cloud.title)}
          badge={t(($) => $.add.cloud.badge)}
          description={t(($) => $.add.cloud.description)}
          cta={t(($) => $.add.cloud.cta)}
          disabled
        />
      </div>

      <div className="flex justify-end">
        <Button variant="ghost" onClick={onClose}>
          {t(($) => $.add.cancel)}
        </Button>
      </div>
    </>
  );
}

function Choice({
  icon,
  title,
  badge,
  description,
  cta,
  ctaIcon,
  onClick,
  href,
  external,
  disabled,
}: {
  icon: React.ReactNode;
  title: string;
  badge?: string;
  description: string;
  cta: string;
  ctaIcon?: React.ReactNode;
  onClick?: () => void;
  // `href` switches the card to an <a target=_blank> for the Web "Download
  // Desktop" affordance (RFC v6.1 §1.3): the card stays visually present so
  // the user knows This Desktop is a real option, but click escapes to a
  // download page instead of pretending we can register a daemon from the
  // browser tab.
  href?: string;
  external?: boolean;
  disabled?: boolean;
}) {
  const body = (
    <>
      <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md border bg-background text-muted-foreground">
        {icon}
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">{title}</span>
          {badge && (
            <Badge variant="secondary" className="text-[10px]">
              {badge}
            </Badge>
          )}
        </div>
        <p className="mt-0.5 truncate text-xs text-muted-foreground">{description}</p>
      </div>
      <span className="flex shrink-0 items-center gap-1 text-xs font-medium text-muted-foreground group-hover:text-foreground">
        {ctaIcon}
        {cta}
      </span>
    </>
  );
  const className = `group flex w-full items-center gap-3 rounded-md border bg-card p-3 text-left transition-colors ${
    disabled
      ? "cursor-not-allowed opacity-60"
      : "hover:border-foreground/20 hover:bg-accent/30"
  }`;
  if (href) {
    return (
      <a
        href={href}
        target={external ? "_blank" : undefined}
        rel={external ? "noopener noreferrer" : undefined}
        className={className}
      >
        {body}
      </a>
    );
  }
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className={className}
    >
      {body}
    </button>
  );
}

// ---------------------------------------------------------------------------
// Step 2: install token + curl one-liner + polling
// ---------------------------------------------------------------------------

function InstallStep({
  knownComputerIds,
  onBack,
  onConnected,
}: {
  knownComputerIds: Set<string>;
  onBack: () => void;
  onConnected: (id: string) => void;
}) {
  const { t } = useT("computers");
  const wsId = useWorkspaceId();
  const slug = useWorkspaceSlug();
  const qc = useQueryClient();
  const [mint, setMint] = useState<InstallTokenMintResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const mintedRef = useRef(false);

  // Mint exactly once when this step mounts. StrictMode double-invokes
  // effects, which would otherwise burn two tokens and surface the second
  // (the first being immediately discarded by React).
  useEffect(() => {
    if (mintedRef.current) return;
    mintedRef.current = true;
    api
      .mintInstallToken()
      .then((m) => setMint(m))
      .catch((e: unknown) => {
        setError(e instanceof Error ? e.message : String(e));
      });
  }, []);

  const command = useMemo(() => {
    if (!mint || !slug) return "";
    return buildInstallCommand({
      workspaceSlug: slug,
      token: mint.token,
      apiBaseUrl: api.getBaseUrl(),
    });
  }, [mint, slug]);

  // Poll computerListOptions while this step is up so a slow / missed WS
  // event still surfaces the new daemon. 3s is short enough to feel
  // responsive without hammering the API. The WS handler also invalidates
  // computerKeys on daemon:* events — polling is a safety net, not the
  // primary signal.
  const { data: computers = [] } = useQuery({
    ...computerListOptions(wsId ?? ""),
    enabled: !!wsId,
    refetchInterval: 3000,
  });

  useEffect(() => {
    const fresh = computers.find((c) => !knownComputerIds.has(c.id));
    if (fresh) {
      toast.success(t(($) => $.page.title));
      qc.invalidateQueries({ queryKey: computerKeys.all(wsId ?? "") });
      onConnected(fresh.id);
    }
  }, [computers, knownComputerIds, onConnected, qc, wsId, t]);

  const handleCopy = useCallback(() => {
    if (!command) return;
    navigator.clipboard.writeText(command).catch(() => {
      /* clipboard write can fail in non-secure contexts; silently no-op */
    });
    setCopied(true);
  }, [command]);

  useEffect(() => {
    if (!copied) return;
    const id = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(id);
  }, [copied]);

  return (
    <>
      <DialogHeader>
        <div className="flex items-center gap-2">
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={onBack}
            aria-label={t(($) => $.add.cancel)}
          >
            <ArrowLeft className="h-3.5 w-3.5" />
          </Button>
          <DialogTitle>{t(($) => $.install.title)}</DialogTitle>
        </div>
      </DialogHeader>

      <div className="space-y-3">
        <div>
          <div className="mb-1 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
            <Terminal className="h-3.5 w-3.5" />
            {t(($) => $.install.step_1)}
          </div>
        </div>

        <div>
          <div className="mb-1 flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
            <Monitor className="h-3.5 w-3.5" />
            {t(($) => $.install.step_2)}
          </div>
          {error ? (
            <div className="rounded-md border border-destructive/30 bg-destructive/5 p-2.5 text-xs text-destructive">
              {error}
            </div>
          ) : !command ? (
            <div className="flex h-20 items-center justify-center rounded-md border bg-muted/30">
              <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />
            </div>
          ) : (
            <div className="relative rounded-md border bg-muted/50">
              <pre className="overflow-x-auto p-2.5 pr-10 font-mono text-xs leading-relaxed text-foreground">
                {command}
              </pre>
              <button
                type="button"
                onClick={handleCopy}
                className="absolute top-1.5 right-1.5 flex h-6 w-6 items-center justify-center rounded border bg-background text-muted-foreground transition-colors hover:text-foreground"
                aria-label={copied ? t(($) => $.install.copied) : t(($) => $.install.copy)}
              >
                {copied ? (
                  <Check className="h-3 w-3 text-success" />
                ) : (
                  <Copy className="h-3 w-3" />
                )}
              </button>
            </div>
          )}
          <p className="mt-1 text-[11px] text-muted-foreground">
            {t(($) => $.install.token_warning)}
          </p>
        </div>

        <div className="flex items-center gap-2 rounded-md border bg-muted/30 px-3 py-2 text-xs">
          <Loader2 className="h-3.5 w-3.5 animate-spin text-muted-foreground" />
          <div className="min-w-0 flex-1">
            <div className="font-medium">{t(($) => $.install.waiting)}</div>
            <div className="text-[11px] text-muted-foreground">
              {t(($) => $.install.polling_hint)}
            </div>
          </div>
        </div>

        <p className="text-[11px] text-muted-foreground">
          {t(($) => $.install.diagnose_hint)}
        </p>
      </div>
    </>
  );
}

export function buildInstallCommand({
  workspaceSlug,
  token,
  apiBaseUrl,
  browserOrigin,
}: {
  workspaceSlug: string;
  token: string;
  apiBaseUrl: string;
  browserOrigin?: string;
}): string {
  const serverUrl = resolveInstallServerUrl(apiBaseUrl, browserOrigin);
  if (!serverUrl) return "";
  return [
    "curl -fsSL https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh",
    `  | bash -s -- --workspace ${shellQuote(workspaceSlug)} --token ${shellQuote(
      token,
    )} --server-url ${shellQuote(serverUrl)}`,
  ].join(" \\\n");
}

function resolveInstallServerUrl(apiBaseUrl: string, browserOrigin?: string): string {
  const trimmed = apiBaseUrl.trim();
  if (/^(https?|wss?):\/\//i.test(trimmed)) {
    return stripTrailingSlashes(trimmed);
  }

  const origin =
    browserOrigin ??
    (typeof window !== "undefined" ? window.location.origin : "");
  if (!origin) return stripTrailingSlashes(trimmed);
  if (!trimmed) return stripTrailingSlashes(origin);
  return stripTrailingSlashes(new URL(trimmed, origin).toString());
}

function stripTrailingSlashes(s: string): string {
  return s.replace(/\/+$/, "");
}

// shellQuote escapes a value so it can be safely interpolated into a shell
// command we're displaying to the user. The slug should already be
// `[a-z0-9-]+`, but quoting defensively means even an unusual self-hosted
// server URL (e.g. one containing `&`) won't break the one-liner.
function shellQuote(s: string): string {
  if (/^[A-Za-z0-9_./:@-]+$/.test(s)) return s;
  return `'${s.replace(/'/g, `'\\''`)}'`;
}
