package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── Response types ──────────────────────────────────────────────────────────

type SquadResponse struct {
	ID          string  `json:"id"`
	WorkspaceID string  `json:"workspace_id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	LeaderID    string  `json:"leader_id"`
	CreatorID   string  `json:"creator_id"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	ArchivedAt  *string `json:"archived_at"`
	ArchivedBy  *string `json:"archived_by"`
}

type SquadMemberResponse struct {
	ID         string `json:"id"`
	SquadID    string `json:"squad_id"`
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
	Role       string `json:"role"`
	CreatedAt  string `json:"created_at"`
}

type SquadActivityLogResponse struct {
	ID               string  `json:"id"`
	SquadID          string  `json:"squad_id"`
	IssueID          string  `json:"issue_id"`
	TriggerCommentID *string `json:"trigger_comment_id"`
	LeaderID         string  `json:"leader_id"`
	Outcome          string  `json:"outcome"`
	Details          any     `json:"details"`
	CreatedAt        string  `json:"created_at"`
}

// ── Converters ──────────────────────────────────────────────────────────────

func squadToResponse(s db.Squad) SquadResponse {
	return SquadResponse{
		ID:          uuidToString(s.ID),
		WorkspaceID: uuidToString(s.WorkspaceID),
		Name:        s.Name,
		Description: s.Description,
		LeaderID:    uuidToString(s.LeaderID),
		CreatorID:   uuidToString(s.CreatorID),
		CreatedAt:   timestampToString(s.CreatedAt),
		UpdatedAt:   timestampToString(s.UpdatedAt),
		ArchivedAt:  timestampToPtr(s.ArchivedAt),
		ArchivedBy:  uuidToPtr(s.ArchivedBy),
	}
}

func squadMemberToResponse(m db.SquadMember) SquadMemberResponse {
	return SquadMemberResponse{
		ID:         uuidToString(m.ID),
		SquadID:    uuidToString(m.SquadID),
		MemberType: m.MemberType,
		MemberID:   uuidToString(m.MemberID),
		Role:       m.Role,
		CreatedAt:  timestampToString(m.CreatedAt),
	}
}

func squadActivityLogToResponse(l db.SquadActivityLog) SquadActivityLogResponse {
	var details any
	if l.Details != nil {
		json.Unmarshal(l.Details, &details)
	}
	return SquadActivityLogResponse{
		ID:               uuidToString(l.ID),
		SquadID:          uuidToString(l.SquadID),
		IssueID:          uuidToString(l.IssueID),
		TriggerCommentID: uuidToPtr(l.TriggerCommentID),
		LeaderID:         uuidToString(l.LeaderID),
		Outcome:          l.Outcome,
		Details:          details,
		CreatedAt:        timestampToString(l.CreatedAt),
	}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// loadSquadInWorkspace loads a squad scoped to the current workspace.
func (h *Handler) loadSquadInWorkspace(w http.ResponseWriter, r *http.Request) (db.Squad, string, bool) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	squadID := chi.URLParam(r, "id")
	squadUUID, ok := parseUUIDOrBadRequest(w, squadID, "squad id")
	if !ok {
		return db.Squad{}, "", false
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return db.Squad{}, "", false
	}
	squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{
		ID:          squadUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "squad not found")
		return db.Squad{}, "", false
	}
	return squad, workspaceID, true
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *Handler) ListSquads(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	squads, err := h.Queries.ListSquads(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list squads")
		return
	}
	resp := make([]SquadResponse, len(squads))
	for i, s := range squads {
		resp[i] = squadToResponse(s)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) CreateSquad(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin")
	if !ok {
		return
	}

	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		LeaderID    string `json:"leader_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.LeaderID == "" {
		writeError(w, http.StatusBadRequest, "leader_id is required")
		return
	}

	leaderUUID, ok := parseUUIDOrBadRequest(w, req.LeaderID, "leader_id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	// Validate leader is an agent in this workspace.
	_, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          leaderUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "leader must be a valid agent in this workspace")
		return
	}

	squad, err := h.Queries.CreateSquad(r.Context(), db.CreateSquadParams{
		WorkspaceID: wsUUID,
		Name:        req.Name,
		Description: req.Description,
		LeaderID:    leaderUUID,
		CreatorID:   member.UserID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "squad name already exists in this workspace")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create squad")
		return
	}

	// Auto-add leader as a member with role "leader".
	h.Queries.AddSquadMember(r.Context(), db.AddSquadMemberParams{
		SquadID:    squad.ID,
		MemberType: "agent",
		MemberID:   leaderUUID,
		Role:       "leader",
	})

	resp := squadToResponse(squad)
	h.publish(protocol.EventSquadCreated, workspaceID, "member", uuidToString(member.UserID), map[string]any{"squad": resp})
	writeJSON(w, http.StatusCreated, resp)
}

func (h *Handler) GetSquad(w http.ResponseWriter, r *http.Request) {
	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, squadToResponse(squad))
}

func (h *Handler) UpdateSquad(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		LeaderID    *string `json:"leader_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	params := db.UpdateSquadParams{ID: squad.ID}
	if req.Name != nil {
		params.Name = pgtype.Text{String: *req.Name, Valid: true}
	}
	if req.Description != nil {
		params.Description = pgtype.Text{String: *req.Description, Valid: true}
	}
	if req.LeaderID != nil {
		lid, ok := parseUUIDOrBadRequest(w, *req.LeaderID, "leader_id")
		if !ok {
			return
		}
		// Validate new leader is an agent in workspace.
		if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
			ID: lid, WorkspaceID: wsUUID,
		}); err != nil {
			writeError(w, http.StatusBadRequest, "leader must be a valid agent in this workspace")
			return
		}
		// Ensure new leader is a squad member; auto-add if not.
		isMember, _ := h.Queries.IsSquadMember(r.Context(), db.IsSquadMemberParams{
			SquadID: squad.ID, MemberType: "agent", MemberID: lid,
		})
		if !isMember {
			h.Queries.AddSquadMember(r.Context(), db.AddSquadMemberParams{
				SquadID: squad.ID, MemberType: "agent", MemberID: lid, Role: "leader",
			})
		}
		params.LeaderID = lid
	}

	updated, err := h.Queries.UpdateSquad(r.Context(), params)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "squad name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update squad")
		return
	}

	resp := squadToResponse(updated)
	h.publish(protocol.EventSquadUpdated, workspaceID, "member", requestUserID(r), map[string]any{"squad": resp})
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) DeleteSquad(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}

	if squad.ArchivedAt.Valid {
		writeError(w, http.StatusBadRequest, "squad is already archived")
		return
	}

	// Transfer issues assigned to this squad to the leader agent.
	if err := h.Queries.TransferSquadAssignees(r.Context(), db.TransferSquadAssigneesParams{
		AssigneeID:   squad.ID,
		AssigneeID_2: squad.LeaderID,
	}); err != nil {
		slog.Warn("transfer squad assignees failed", "squad_id", uuidToString(squad.ID), "error", err)
	}

	userID := requestUserID(r)
	userUUID, _ := parseUUIDOrBadRequest(w, userID, "user_id")

	if _, err := h.Queries.ArchiveSquad(r.Context(), db.ArchiveSquadParams{
		ID:         squad.ID,
		ArchivedBy: userUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to archive squad")
		return
	}

	h.publish(protocol.EventSquadDeleted, workspaceID, "member", userID, map[string]any{
		"squad_id":  uuidToString(squad.ID),
		"leader_id": uuidToString(squad.LeaderID),
	})
	w.WriteHeader(http.StatusNoContent)
}

// ── Squad Members ───────────────────────────────────────────────────────────

func (h *Handler) ListSquadMembers(w http.ResponseWriter, r *http.Request) {
	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	members, err := h.Queries.ListSquadMembers(r.Context(), squad.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list squad members")
		return
	}
	resp := make([]SquadMemberResponse, len(members))
	for i, m := range members {
		resp[i] = squadMemberToResponse(m)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) AddSquadMember(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	var req struct {
		MemberType string `json:"member_type"`
		MemberID   string `json:"member_id"`
		Role       string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MemberType != "agent" && req.MemberType != "member" {
		writeError(w, http.StatusBadRequest, "member_type must be 'agent' or 'member'")
		return
	}
	if req.MemberID == "" {
		writeError(w, http.StatusBadRequest, "member_id is required")
		return
	}

	memberUUID, ok := parseUUIDOrBadRequest(w, req.MemberID, "member_id")
	if !ok {
		return
	}

	// Validate the member belongs to this workspace.
	if req.MemberType == "agent" {
		if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
			ID: memberUUID, WorkspaceID: wsUUID,
		}); err != nil {
			writeError(w, http.StatusBadRequest, "agent not found in this workspace")
			return
		}
	} else {
		if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
			UserID: memberUUID, WorkspaceID: wsUUID,
		}); err != nil {
			writeError(w, http.StatusBadRequest, "member not found in this workspace")
			return
		}
	}

	sm, err := h.Queries.AddSquadMember(r.Context(), db.AddSquadMemberParams{
		SquadID:    squad.ID,
		MemberType: req.MemberType,
		MemberID:   memberUUID,
		Role:       req.Role,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "member already in squad")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to add squad member")
		return
	}

	writeJSON(w, http.StatusCreated, squadMemberToResponse(sm))
}

func (h *Handler) RemoveSquadMember(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}

	var req struct {
		MemberType string `json:"member_type"`
		MemberID   string `json:"member_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	memberUUID, ok := parseUUIDOrBadRequest(w, req.MemberID, "member_id")
	if !ok {
		return
	}

	// Prevent removing the leader.
	if req.MemberType == "agent" && uuidToString(squad.LeaderID) == req.MemberID {
		writeError(w, http.StatusBadRequest, "cannot remove the squad leader; change leader first")
		return
	}

	if err := h.Queries.RemoveSquadMember(r.Context(), db.RemoveSquadMemberParams{
		SquadID:    squad.ID,
		MemberType: req.MemberType,
		MemberID:   memberUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove squad member")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) UpdateSquadMemberRole(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	if _, ok := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin"); !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}

	var req struct {
		MemberType string `json:"member_type"`
		MemberID   string `json:"member_id"`
		Role       string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	memberUUID, ok := parseUUIDOrBadRequest(w, req.MemberID, "member_id")
	if !ok {
		return
	}

	sm, err := h.Queries.UpdateSquadMemberRole(r.Context(), db.UpdateSquadMemberRoleParams{
		SquadID:    squad.ID,
		MemberType: req.MemberType,
		MemberID:   memberUUID,
		Role:       req.Role,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "squad member not found")
		return
	}

	writeJSON(w, http.StatusOK, squadMemberToResponse(sm))
}

// ── Squad Activity Log ──────────────────────────────────────────────────────

func (h *Handler) ListSquadActivityLogs(w http.ResponseWriter, r *http.Request) {
	issueID := chi.URLParam(r, "issueId")
	// Validate issue belongs to current workspace.
	issue, ok := h.loadIssueForUser(w, r, issueID)
	if !ok {
		return
	}

	logs, err := h.Queries.ListSquadActivityLogs(r.Context(), db.ListSquadActivityLogsParams{
		IssueID: issue.ID,
		Limit:   50,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list squad activity logs")
		return
	}

	resp := make([]SquadActivityLogResponse, len(logs))
	for i, l := range logs {
		resp[i] = squadActivityLogToResponse(l)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) CreateSquadActivityLog(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	var req struct {
		SquadID          string `json:"squad_id"`
		IssueID          string `json:"issue_id"`
		TriggerCommentID string `json:"trigger_comment_id"`
		Outcome          string `json:"outcome"`
		Details          any    `json:"details"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Outcome != "action" && req.Outcome != "no_action" && req.Outcome != "failed" {
		writeError(w, http.StatusBadRequest, "outcome must be 'action', 'no_action', or 'failed'")
		return
	}

	squadUUID, ok := parseUUIDOrBadRequest(w, req.SquadID, "squad_id")
	if !ok {
		return
	}
	issueUUID, ok := parseUUIDOrBadRequest(w, req.IssueID, "issue_id")
	if !ok {
		return
	}

	// Validate squad belongs to this workspace.
	squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{
		ID:          squadUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "squad not found in this workspace")
		return
	}

	// Validate issue belongs to this workspace.
	if _, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
		ID:          issueUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusNotFound, "issue not found in this workspace")
		return
	}

	// Security: only the squad leader agent can write activity logs.
	userID := requestUserID(r)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if actorType != "agent" || actorID != uuidToString(squad.LeaderID) {
		writeError(w, http.StatusForbidden, "only the squad leader agent can record activity")
		return
	}

	var triggerCommentUUID pgtype.UUID
	if req.TriggerCommentID != "" {
		triggerCommentUUID, ok = parseUUIDOrBadRequest(w, req.TriggerCommentID, "trigger_comment_id")
		if !ok {
			return
		}
	}

	var detailsJSON []byte
	if req.Details != nil {
		detailsJSON, _ = json.Marshal(req.Details)
	}
	if detailsJSON == nil {
		detailsJSON = []byte("{}")
	}

	log, err := h.Queries.CreateSquadActivityLog(r.Context(), db.CreateSquadActivityLogParams{
		SquadID:          squadUUID,
		IssueID:          issueUUID,
		TriggerCommentID: triggerCommentUUID,
		LeaderID:         squad.LeaderID,
		Outcome:          req.Outcome,
		Details:          detailsJSON,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create squad activity log")
		return
	}

	writeJSON(w, http.StatusCreated, squadActivityLogToResponse(log))
}

// ── Squad Trigger Logic ─────────────────────────────────────────────────────

// shouldEnqueueSquadLeaderOnComment returns true if the issue is assigned to a
// squad and the comment author is NOT a member of that squad (anti-loop).
func (h *Handler) shouldEnqueueSquadLeaderOnComment(ctx context.Context, issue db.Issue, authorType, authorID string) bool {
	if !issue.AssigneeType.Valid || issue.AssigneeType.String != "squad" || !issue.AssigneeID.Valid {
		return false
	}

	// Load the squad.
	squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
		ID:          issue.AssigneeID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return false
	}

	// Check if the author is a squad member (anti-loop: squad members don't trigger leader).
	isMember, err := h.Queries.IsSquadMember(ctx, db.IsSquadMemberParams{
		SquadID:    squad.ID,
		MemberType: authorType,
		MemberID:   parseUUID(authorID),
	})
	if err == nil && isMember {
		return false
	}

	// Verify leader agent is ready (has runtime, not archived).
	agent, err := h.Queries.GetAgent(ctx, squad.LeaderID)
	if err != nil || !agent.RuntimeID.Valid || agent.ArchivedAt.Valid {
		return false
	}

	return true
}

// enqueueSquadLeaderTask triggers the squad leader agent for an issue assigned to a squad.
func (h *Handler) enqueueSquadLeaderTask(ctx context.Context, issue db.Issue, triggerCommentID pgtype.UUID, authorType, authorID string) {
	squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
		ID:          issue.AssigneeID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return
	}

	// Dedup: skip if leader already has a pending task for this issue.
	hasPending, err := h.Queries.HasPendingTaskForIssueAndAgent(ctx, db.HasPendingTaskForIssueAndAgentParams{
		IssueID: issue.ID,
		AgentID: squad.LeaderID,
	})
	if err != nil || hasPending {
		return
	}

	if _, err := h.TaskService.EnqueueTaskForMention(ctx, issue, squad.LeaderID, triggerCommentID); err != nil {
		slog.Warn("enqueue squad leader task failed",
			"issue_id", uuidToString(issue.ID),
			"squad_id", uuidToString(squad.ID),
			"leader_id", uuidToString(squad.LeaderID),
			"error", err)
	}
}
