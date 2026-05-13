"use client";

import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { useCurrentWorkspace, useWorkspacePaths } from "@multica/core/paths";
import { useWorkspaceId } from "@multica/core/hooks";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import { useNavigation } from "../../navigation";
import { AppLink } from "../../navigation";
import { PageHeader } from "../../layout/page-header";
import { Users, Plus, Trash2, ArrowLeft, Crown } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Label } from "@multica/ui/components/ui/label";
import { ActorAvatar } from "../../common/actor-avatar";
import { toast } from "sonner";
import type { Squad, SquadMember, Agent } from "@multica/core/types";

export function SquadDetailPage() {
  const workspace = useCurrentWorkspace();
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { pathname, push } = useNavigation();
  const queryClient = useQueryClient();
  const squadId = pathname.split("/").pop() ?? "";

  const { data: squad } = useQuery<Squad>({
    queryKey: ["squad", workspace?.id, squadId],
    queryFn: () => api.getSquad(squadId),
    enabled: !!workspace?.id && !!squadId,
  });

  const { data: members = [], refetch: refetchMembers } = useQuery<SquadMember[]>({
    queryKey: ["squad-members", workspace?.id, squadId],
    queryFn: () => api.listSquadMembers(squadId),
    enabled: !!workspace?.id && !!squadId,
  });

  const { data: agents = [] } = useQuery(agentListOptions(wsId));
  const { data: wsMembers = [] } = useQuery(memberListOptions(wsId));

  const [showAddMember, setShowAddMember] = useState(false);
  const [addType, setAddType] = useState<"agent" | "member">("agent");
  const [addId, setAddId] = useState("");
  const [addDescription, setAddDescription] = useState("");

  const addMemberMut = useMutation({
    mutationFn: () => api.addSquadMember(squadId, { member_type: addType, member_id: addId, role: addDescription }),
    onSuccess: () => { refetchMembers(); setShowAddMember(false); setAddId(""); setAddDescription(""); toast.success("Member added"); },
    onError: () => toast.error("Failed to add member"),
  });

  const removeMemberMut = useMutation({
    mutationFn: (m: SquadMember) => api.removeSquadMember(squadId, { member_type: m.member_type, member_id: m.member_id }),
    onSuccess: () => { refetchMembers(); toast.success("Member removed"); },
    onError: () => toast.error("Failed to remove member"),
  });

  const deleteMut = useMutation({
    mutationFn: () => api.deleteSquad(squadId),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ["squads"] }); push(p.squads()); toast.success("Squad archived"); },
    onError: () => toast.error("Failed to archive squad"),
  });

  const getEntityName = (type: string, id: string) => {
    if (type === "agent") return agents.find((a: Agent) => a.id === id)?.name ?? id.slice(0, 8);
    return wsMembers.find((m) => m.user_id === id)?.name ?? id.slice(0, 8);
  };

  if (!squad) {
    return <div className="p-6 text-muted-foreground text-sm">Loading...</div>;
  }

  const availableAgents = agents.filter((a: Agent) => !a.archived_at && !members.some((m) => m.member_type === "agent" && m.member_id === a.id));
  const availableMembers = wsMembers.filter((m) => !members.some((sm) => sm.member_type === "member" && sm.member_id === m.user_id));
  const isLeader = (m: SquadMember) => m.member_type === "agent" && squad.leader_id === m.member_id;

  return (
    <div className="flex flex-1 flex-col">
      <PageHeader className="justify-between px-5">
        <div className="flex items-center gap-2">
          <AppLink href={p.squads()} className="text-muted-foreground hover:text-foreground">
            <ArrowLeft className="h-4 w-4" />
          </AppLink>
          <Users className="h-4 w-4 text-muted-foreground" />
          <h1 className="text-sm font-medium">{squad.name}</h1>
        </div>
        <Button size="sm" variant="ghost" className="text-destructive hover:text-destructive" onClick={() => { if (confirm("Archive this squad? Issues will be transferred to the leader.")) deleteMut.mutate(); }}>
          <Trash2 className="size-3.5 mr-1" />
          Archive
        </Button>
      </PageHeader>

      <div className="flex-1 overflow-y-auto">
        <div className="max-w-2xl mx-auto p-6 space-y-8">
          {/* Squad Info */}
          {squad.description && (
            <div>
              <Label className="text-xs text-muted-foreground">Description</Label>
              <p className="mt-1 text-sm">{squad.description}</p>
            </div>
          )}

          {/* Members Section */}
          <div>
            <div className="flex items-center justify-between mb-4">
              <div>
                <h3 className="text-sm font-medium">Members</h3>
                <p className="text-xs text-muted-foreground mt-0.5">{members.length} member{members.length !== 1 ? "s" : ""} in this squad</p>
              </div>
              <Button size="sm" variant="outline" onClick={() => setShowAddMember(true)}>
                <Plus className="size-3.5 mr-1.5" />
                Add Member
              </Button>
            </div>

            {/* Add Member Form */}
            {showAddMember && (
              <div className="rounded-lg border p-4 mb-4 space-y-3">
                <div className="flex gap-3">
                  <div className="w-28">
                    <Label className="text-xs text-muted-foreground">Type</Label>
                    <select
                      value={addType}
                      onChange={(e) => { setAddType(e.target.value as "agent" | "member"); setAddId(""); }}
                      className="mt-1 w-full rounded-md border bg-transparent px-2 py-1.5 text-sm"
                    >
                      <option value="agent">Agent</option>
                      <option value="member">Member</option>
                    </select>
                  </div>
                  <div className="flex-1">
                    <Label className="text-xs text-muted-foreground">{addType === "agent" ? "Agent" : "Member"}</Label>
                    <select
                      value={addId}
                      onChange={(e) => setAddId(e.target.value)}
                      className="mt-1 w-full rounded-md border bg-transparent px-2 py-1.5 text-sm"
                    >
                      <option value="">Select...</option>
                      {addType === "agent"
                        ? availableAgents.map((a: Agent) => <option key={a.id} value={a.id}>{a.name}</option>)
                        : availableMembers.map((m) => <option key={m.user_id} value={m.user_id}>{m.name}</option>)
                      }
                    </select>
                  </div>
                </div>
                <div>
                  <Label className="text-xs text-muted-foreground">Description</Label>
                  <textarea
                    value={addDescription}
                    onChange={(e) => setAddDescription(e.target.value)}
                    placeholder="Describe this member's role and responsibilities in the squad..."
                    rows={2}
                    className="mt-1 w-full rounded-md border bg-transparent px-3 py-2 text-sm outline-none resize-none focus:ring-1 focus:ring-ring"
                  />
                </div>
                <div className="flex gap-2">
                  <Button size="sm" onClick={() => addMemberMut.mutate()} disabled={!addId || addMemberMut.isPending}>
                    {addMemberMut.isPending ? "Adding..." : "Add"}
                  </Button>
                  <Button size="sm" variant="ghost" onClick={() => { setShowAddMember(false); setAddId(""); setAddDescription(""); }}>Cancel</Button>
                </div>
              </div>
            )}

            {/* Member List */}
            <div className="space-y-2">
              {members.map((m) => (
                <div key={m.id} className="flex items-start gap-3 rounded-lg border p-3">
                  <ActorAvatar actorType={m.member_type} actorId={m.member_id} size={32} showStatusDot />
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{getEntityName(m.member_type, m.member_id)}</span>
                      <span className="text-xs text-muted-foreground capitalize">{m.member_type}</span>
                      {isLeader(m) && (
                        <span className="inline-flex items-center gap-0.5 text-xs bg-amber-100 dark:bg-amber-900/30 text-amber-700 dark:text-amber-400 px-1.5 py-0.5 rounded">
                          <Crown className="size-3" />
                          Leader
                        </span>
                      )}
                    </div>
                    {m.role && (
                      <p className="text-xs text-muted-foreground mt-0.5">{m.role}</p>
                    )}
                  </div>
                  {!isLeader(m) && (
                    <Button
                      size="sm"
                      variant="ghost"
                      className="text-muted-foreground hover:text-destructive h-8 w-8 p-0"
                      onClick={() => removeMemberMut.mutate(m)}
                    >
                      <Trash2 className="size-3.5" />
                    </Button>
                  )}
                </div>
              ))}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
