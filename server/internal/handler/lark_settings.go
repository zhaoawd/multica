package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Lark binding settings — see LARK_INTEGRATION_DESIGN.md §6.7.
//
// Endpoints exposed:
//   GET    /api/workspaces/{id}/lark/binding   → current binding (or {bound:false})
//   POST   /api/workspaces/{id}/lark/binding   → upsert chat_id + enabled_events
//   PATCH  /api/workspaces/{id}/lark/binding   → enabled_events only
//   DELETE /api/workspaces/{id}/lark/binding   → unbind
//
// Per-route admin gating is applied at the router level (see router.go).
//
// Configuration source-of-truth: env vars (LARK_APP_ID, LARK_APP_SECRET,
// LARK_VERIFICATION_TOKEN, LARK_ENCRYPT_KEY). The settings UI surfaces
// the `Configured` flag from /binding so admins know whether the Connect
// button will succeed without redundant /connect calls.

// LarkBindingResponse is the JSON shape returned for binding endpoints.
// When `Bound` is false, all other fields except `Configured` are zero
// values — the front-end uses Bound to decide between Connect and Edit
// affordances.
type LarkBindingResponse struct {
	Bound         bool     `json:"bound"`
	Configured    bool     `json:"configured"`
	ChatID        string   `json:"chat_id,omitempty"`
	EnabledEvents []string `json:"enabled_events"`
	CreatedAt     string   `json:"created_at,omitempty"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
	// SupportedEvents enumerates the event keys this server knows how to
	// render. The UI builds its checklist from this list so adding a new
	// card on the server propagates to the UI without a frontend change.
	SupportedEvents []string `json:"supported_events"`
}

type LarkBindingUpsertRequest struct {
	ChatID        string   `json:"chat_id"`
	EnabledEvents []string `json:"enabled_events"`
}

type LarkBindingPatchRequest struct {
	EnabledEvents []string `json:"enabled_events"`
}

// supportedLarkEvents is the canonical list of event kinds the notifier
// can render. Order matters — the UI renders the checklist in this order.
var supportedLarkEvents = []string{
	protocol.EventIssueCreated,
	protocol.EventIssueUpdated,
	protocol.EventTaskCompleted,
	protocol.EventTaskFailed,
	protocol.EventCommentCreated,
}

func isSupportedLarkEvent(s string) bool {
	for _, e := range supportedLarkEvents {
		if e == s {
			return true
		}
	}
	return false
}

func bindingToResponse(b db.LarkWorkspaceBinding, configured bool) LarkBindingResponse {
	events := b.EnabledEvents
	if events == nil {
		events = []string{}
	}
	return LarkBindingResponse{
		Bound:           true,
		Configured:      configured,
		ChatID:          b.ChatID,
		EnabledEvents:   events,
		CreatedAt:       timestampToString(b.CreatedAt),
		UpdatedAt:       timestampToString(b.UpdatedAt),
		SupportedEvents: supportedLarkEvents,
	}
}

// GetLarkBinding returns the workspace's binding (or {bound:false}).
func (h *Handler) GetLarkBinding(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	configured := service.LarkConfigFromEnv().Configured()
	binding, err := h.Queries.GetLarkWorkspaceBinding(r.Context(), wsUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, LarkBindingResponse{
				Bound:           false,
				Configured:      configured,
				EnabledEvents:   []string{},
				SupportedEvents: supportedLarkEvents,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load binding")
		return
	}
	writeJSON(w, http.StatusOK, bindingToResponse(binding, configured))
}

// UpsertLarkBinding creates or replaces the binding.
// Empty chat_id is rejected (use DELETE to unbind). Unknown event kinds in
// enabled_events are dropped silently so older clients sending stale keys
// don't 400 the request.
func (h *Handler) UpsertLarkBinding(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	cfg := service.LarkConfigFromEnv()
	if !cfg.Configured() {
		writeError(w, http.StatusFailedDependency, "lark integration is not configured on this server")
		return
	}
	var req LarkBindingUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ChatID == "" {
		writeError(w, http.StatusBadRequest, "chat_id required")
		return
	}
	events := filterSupportedEvents(req.EnabledEvents)
	binding, err := h.Queries.UpsertLarkWorkspaceBinding(r.Context(), db.UpsertLarkWorkspaceBindingParams{
		WorkspaceID:   wsUUID,
		ChatID:        req.ChatID,
		BotTokenEnc:   []byte{},
		EnabledEvents: events,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save binding")
		return
	}
	writeJSON(w, http.StatusOK, bindingToResponse(binding, true))
}

// PatchLarkBinding updates enabled_events only.
func (h *Handler) PatchLarkBinding(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	var req LarkBindingPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	events := filterSupportedEvents(req.EnabledEvents)
	binding, err := h.Queries.UpdateLarkWorkspaceBindingEvents(r.Context(), db.UpdateLarkWorkspaceBindingEventsParams{
		WorkspaceID:   wsUUID,
		EnabledEvents: events,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "binding not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update binding")
		return
	}
	writeJSON(w, http.StatusOK, bindingToResponse(binding, service.LarkConfigFromEnv().Configured()))
}

// DeleteLarkBinding removes the binding. Idempotent — returns 204 even
// when no row existed (matches the GitHub installation delete shape).
func (h *Handler) DeleteLarkBinding(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, wsID, "workspace id")
	if !ok {
		return
	}
	if err := h.Queries.DeleteLarkWorkspaceBinding(r.Context(), wsUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete binding")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func filterSupportedEvents(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, ev := range in {
		if !isSupportedLarkEvent(ev) || seen[ev] {
			continue
		}
		seen[ev] = true
		out = append(out, ev)
	}
	return out
}
