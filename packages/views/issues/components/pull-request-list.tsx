"use client";

import { useQuery } from "@tanstack/react-query";
import {
  CheckCircle2,
  CircleDashed,
  GitMerge,
  GitPullRequest,
  GitPullRequestArrow,
  GitPullRequestClosed,
  GitPullRequestDraft,
  TriangleAlert,
  XCircle,
} from "lucide-react";
import { issuePullRequestsOptions } from "@multica/core/github/queries";
import type {
  GitHubPullRequest,
  GitHubPullRequestChecksConclusion,
  GitHubPullRequestState,
} from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

const STATE_ICON: Record<
  GitHubPullRequestState,
  { icon: React.ComponentType<{ className?: string }>; className: string }
> = {
  open: { icon: GitPullRequestArrow, className: "text-emerald-600 dark:text-emerald-400" },
  draft: { icon: GitPullRequestDraft, className: "text-muted-foreground" },
  merged: { icon: GitMerge, className: "text-violet-600 dark:text-violet-400" },
  closed: { icon: GitPullRequestClosed, className: "text-rose-600 dark:text-rose-400" },
};

const CHECKS_ICON: Record<
  GitHubPullRequestChecksConclusion,
  { icon: React.ComponentType<{ className?: string }>; className: string }
> = {
  passed: { icon: CheckCircle2, className: "text-emerald-600 dark:text-emerald-400" },
  failed: { icon: XCircle, className: "text-rose-600 dark:text-rose-400" },
  pending: { icon: CircleDashed, className: "text-amber-600 dark:text-amber-400" },
};

export function PullRequestList({ issueId }: { issueId: string }) {
  const { t } = useT("issues");
  const { data, isLoading } = useQuery(issuePullRequestsOptions(issueId));
  const prs = data?.pull_requests ?? [];

  if (isLoading) {
    return <p className="text-xs text-muted-foreground px-2">{t(($) => $.detail.pull_requests_loading)}</p>;
  }
  if (prs.length === 0) {
    return (
      <p className="text-xs text-muted-foreground px-2">
        {t(($) => $.detail.pull_requests_empty)}
      </p>
    );
  }

  return (
    <div className="space-y-1">
      {prs.map((pr) => (
        <PullRequestRow key={pr.id} pr={pr} />
      ))}
    </div>
  );
}

function PullRequestRow({ pr }: { pr: GitHubPullRequest }) {
  const { t } = useT("issues");
  const cfg = STATE_ICON[pr.state] ?? { icon: GitPullRequest, className: "" };
  const Icon = cfg.icon;
  const label =
    pr.state === "open"
      ? t(($) => $.detail.pull_request_state_open)
      : pr.state === "draft"
        ? t(($) => $.detail.pull_request_state_draft)
        : pr.state === "merged"
          ? t(($) => $.detail.pull_request_state_merged)
          : pr.state === "closed"
            ? t(($) => $.detail.pull_request_state_closed)
            : pr.state;
  // Hide the status row entirely for terminal PRs — closed/merged PRs don't
  // change CI or conflict state anymore so the badges are noise.
  const showStatus = pr.state === "open" || pr.state === "draft";
  return (
    <a
      href={pr.html_url}
      target="_blank"
      rel="noreferrer noopener"
      className="flex items-start gap-2 rounded-md px-2 py-1.5 -mx-2 hover:bg-accent/50 transition-colors group"
    >
      <Icon className={cn("h-3.5 w-3.5 mt-0.5 shrink-0", cfg.className)} />
      <div className="min-w-0 flex-1">
        <p className="text-xs font-medium truncate group-hover:text-foreground">{pr.title}</p>
        <p className="text-[11px] text-muted-foreground truncate">
          {pr.repo_owner}/{pr.repo_name}#{pr.number} · {label}
          {pr.author_login ? ` · @${pr.author_login}` : null}
        </p>
        {showStatus ? <PullRequestStatusRow pr={pr} /> : null}
      </div>
    </a>
  );
}

function PullRequestStatusRow({ pr }: { pr: GitHubPullRequest }) {
  const { t } = useT("issues");
  const checks = pr.checks_conclusion ?? null;
  const mergeable = pr.mergeable_state ?? null;
  // Conflicts: only assert state for `clean` / `dirty`. Other GitHub values
  // (`blocked`, `behind`, `unstable`, `unknown`, `has_hooks`, `draft`) mean
  // "not mergeable but not necessarily a conflict" — surfacing them as
  // conflicts would mislead the user.
  const conflictsBadge =
    mergeable === "dirty"
      ? { icon: TriangleAlert, label: t(($) => $.detail.pull_request_conflicts_dirty), className: "text-rose-600 dark:text-rose-400" }
      : mergeable === "clean"
        ? { icon: CheckCircle2, label: t(($) => $.detail.pull_request_conflicts_clean), className: "text-emerald-600 dark:text-emerald-400" }
        : null;
  const checksBadge =
    checks && CHECKS_ICON[checks]
      ? {
          icon: CHECKS_ICON[checks].icon,
          className: CHECKS_ICON[checks].className,
          label:
            checks === "passed"
              ? t(($) => $.detail.pull_request_checks_passed)
              : checks === "failed"
                ? t(($) => $.detail.pull_request_checks_failed)
                : t(($) => $.detail.pull_request_checks_pending),
        }
      : null;
  if (!conflictsBadge && !checksBadge) return null;
  return (
    <div className="flex items-center gap-3 mt-0.5">
      {checksBadge ? (
        <span className="flex items-center gap-1 text-[11px] text-muted-foreground">
          <checksBadge.icon className={cn("h-3 w-3", checksBadge.className)} />
          {checksBadge.label}
        </span>
      ) : null}
      {conflictsBadge ? (
        <span className="flex items-center gap-1 text-[11px] text-muted-foreground">
          <conflictsBadge.icon className={cn("h-3 w-3", conflictsBadge.className)} />
          {conflictsBadge.label}
        </span>
      ) : null}
    </div>
  );
}
