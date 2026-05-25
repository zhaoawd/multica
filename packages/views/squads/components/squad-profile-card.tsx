"use client";

import { useQuery } from "@tanstack/react-query";
import type { SquadMemberStatus } from "@multica/core/types";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  squadListOptions,
  squadMemberStatusOptions,
  agentListOptions,
  memberListOptions,
} from "@multica/core/workspace/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import { ActorAvatar as ActorAvatarBase } from "@multica/ui/components/common/actor-avatar";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { ActorAvatar } from "../../common/actor-avatar";
import { AppLink } from "../../navigation";
import { useT } from "../../i18n";

interface SquadProfileCardProps {
  squadId: string;
}

const STATUS_DOT_CLASS: Record<string, string> = {
  working: "bg-success",
  idle: "bg-muted-foreground/40",
  offline: "bg-muted-foreground/40",
  unstable: "bg-warning",
};

export function SquadProfileCard({ squadId }: SquadProfileCardProps) {
  const { t } = useT("squads");
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { data: squads = [], isLoading: squadsLoading } = useQuery(
    squadListOptions(wsId),
  );
  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: wsMembers = [] } = useQuery(memberListOptions(wsId));
  const { data: memberStatusResp } = useQuery(
    squadMemberStatusOptions(wsId, squadId),
  );

  const squad = squads.find((s) => s.id === squadId);

  if (squadsLoading && !squad) {
    return (
      <div className="flex items-center gap-3">
        <Skeleton className="h-10 w-10 rounded-md" />
        <div className="flex-1 space-y-1.5">
          <Skeleton className="h-4 w-28" />
          <Skeleton className="h-3 w-20" />
        </div>
      </div>
    );
  }

  if (!squad) {
    return (
      <div className="text-xs text-muted-foreground">
        {t(($) => $.profile_card.unavailable)}
      </div>
    );
  }

  const isArchived = !!squad.archived_at;
  const initials = squad.name
    .split(" ")
    .map((w) => w[0])
    .join("")
    .toUpperCase()
    .slice(0, 2);

  const memberStatuses = memberStatusResp?.members ?? [];
  const leaderFirst = [...memberStatuses].sort((a, b) => {
    const aLeader = a.member_type === "agent" && a.member_id === squad.leader_id;
    const bLeader = b.member_type === "agent" && b.member_id === squad.leader_id;
    if (aLeader && !bLeader) return -1;
    if (!aLeader && bLeader) return 1;
    return 0;
  });

  return (
    <div className="group flex flex-col gap-3 text-left">
      <div className="flex items-start gap-3">
        <ActorAvatarBase
          name={squad.name}
          initials={initials}
          avatarUrl={squad.avatar_url}
          isSquad
          size={40}
          className="rounded-md"
        />
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5">
            <p className="truncate text-sm font-semibold">{squad.name}</p>
            {isArchived && (
              <span className="rounded-md bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
                {t(($) => $.profile_card.archived)}
              </span>
            )}
          </div>
        </div>
        {!isArchived && (
          <AppLink
            href={p.squadDetail(squad.id)}
            className="mr-1 mt-0.5 shrink-0 text-xs font-normal text-brand opacity-0 transition-opacity group-hover:opacity-100"
          >
            {t(($) => $.profile_card.detail_link)}
          </AppLink>
        )}
      </div>

      {squad.description && (
        <p className="line-clamp-2 text-xs text-muted-foreground">
          {squad.description}
        </p>
      )}

      {leaderFirst.length > 0 && (
        <MembersList
          members={leaderFirst}
          leaderId={squad.leader_id}
          agents={agents}
          wsMembers={wsMembers}
        />
      )}
    </div>
  );
}

function MembersList({
  members,
  leaderId,
  agents,
  wsMembers,
}: {
  members: SquadMemberStatus[];
  leaderId: string;
  agents: { id: string; name: string }[];
  wsMembers: { user_id: string; name: string; role: string }[];
}) {
  const { t } = useT("squads");
  const p = useWorkspacePaths();
  const visible = members.slice(0, 3);
  const overflow = members.length - visible.length;

  return (
    <div className="flex flex-col gap-1.5 text-xs">
      <span className="text-muted-foreground">
        {t(($) => $.profile_card.members_section)}
        <span className="ml-1 tabular-nums">· {members.length}</span>
      </span>
      <div className="flex max-h-[132px] flex-col gap-0.5 overflow-y-auto">
        {visible.map((m) => {
          const isLeader =
            m.member_type === "agent" && m.member_id === leaderId;
          const name =
            m.member_type === "agent"
              ? agents.find((a) => a.id === m.member_id)?.name ??
                m.member_id.slice(0, 8)
              : wsMembers.find((u) => u.user_id === m.member_id)?.name ??
                m.member_id.slice(0, 8);
          const statusDotClass =
            m.status && m.status in STATUS_DOT_CLASS
              ? STATUS_DOT_CLASS[m.status]
              : null;
          const statusLabel =
            m.status === "working"
              ? t(($) => $.members_tab.status_working)
              : m.status === "idle"
                ? t(($) => $.members_tab.status_idle)
                : m.status === "offline"
                  ? t(($) => $.members_tab.status_offline)
                  : m.status === "unstable"
                    ? t(($) => $.members_tab.status_unstable)
                    : null;
          const href =
            m.member_type === "agent"
              ? p.agentDetail(m.member_id)
              : p.memberDetail(m.member_id);
          const memberRole =
            m.member_type === "member"
              ? wsMembers.find((u) => u.user_id === m.member_id)?.role ?? null
              : null;

          return (
            <AppLink
              key={`${m.member_type}-${m.member_id}`}
              href={href}
              className="-mx-1 flex items-center gap-2 rounded-md px-1 py-1 transition-colors hover:bg-accent"
            >
              <ActorAvatar
                actorType={m.member_type}
                actorId={m.member_id}
                size={20}
                showStatusDot={m.member_type === "agent"}
                className="shrink-0"
              />
              <span className="min-w-0 truncate font-medium">{name}</span>
              {isLeader && (
                <span className="shrink-0 rounded-md bg-amber-100 px-1 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/30 dark:text-amber-400">
                  {t(($) => $.members_tab.leader_chip)}
                </span>
              )}
              {m.member_type === "agent" && statusLabel && (
                <span className="ml-auto inline-flex shrink-0 items-center gap-1 text-muted-foreground">
                  <span
                    className={`h-1.5 w-1.5 rounded-full ${statusDotClass ?? "bg-muted-foreground/40"}`}
                  />
                  {statusLabel}
                </span>
              )}
              {m.member_type === "member" && memberRole && (
                <span className="ml-auto shrink-0 text-muted-foreground">
                  {memberRole}
                </span>
              )}
            </AppLink>
          );
        })}
        {overflow > 0 && (
          <span className="text-muted-foreground">
            {t(($) => $.profile_card.more_members, { count: overflow })}
          </span>
        )}
      </div>
    </div>
  );
}
