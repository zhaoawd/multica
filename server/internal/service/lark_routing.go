package service

import "github.com/multica-ai/multica/server/pkg/protocol"

// Lark routing decision — pure function. The §6.1 routing table is the
// command-and-control of the integration: get a cell wrong and the team
// chat fills with personal noise, or the assignee never sees their work.
//
// This file declares (a) the canonical set of Lark-aware events the
// router knows about, and (b) the routing function that maps an event +
// payload-derived conditions to a list of channels and a card kind. The
// function is exercised by lark_routing_test.go against the matrix in
// testdata/routing_matrix.golden.yaml — that golden is the contract.
//
// IMPORTANT: This function is not yet wired into the live dispatch path.
// The current outbound path (lark_listeners.go + LarkNotify.dispatch)
// only knows about a single bound team chat. Wiring DM and thread
// channels through this decision lives in P2/P3 — but the design intent
// is pinned here today so the implementation can be reviewed against
// the table, not against a re-derivation in PR diffs.

// LarkChannel is a destination the router can pick.
type LarkChannel string

const (
	// LarkChannelTeam: the workspace's bound team chat (chat_id from
	// lark_workspace_binding). Reserved for "team must collectively
	// notice" signals — unassigned issues, public blockers.
	LarkChannelTeam LarkChannel = "team"

	// LarkChannelDM: the assignee / recipient's private chat with the
	// bot. Routed via lark_user_link.open_id. Where individual action
	// signals live.
	LarkChannelDM LarkChannel = "dm"

	// LarkChannelThreadReply: a reply into the originating Lark thread
	// (anchored on lark_issue_link.chat_id + root_message_id).
	LarkChannelThreadReply LarkChannel = "thread_reply"
)

// LarkCardKind labels the rendered card so the test matrix can assert
// "this event picks the claim card, not the assigned card". The actual
// card layout lives in lark_notify.go's build* helpers.
type LarkCardKind string

const (
	LarkCardNone                 LarkCardKind = ""
	LarkCardClaim                LarkCardKind = "claim_card"
	LarkCardCreatedConfirmation  LarkCardKind = "created_confirmation"
	LarkCardAssigned             LarkCardKind = "assigned_card"
	LarkCardPublicBlocker        LarkCardKind = "public_blocker"
	LarkCardTaskCompleted        LarkCardKind = "task_completed"
	LarkCardTaskFailedPersonal   LarkCardKind = "task_failed_personal"
	LarkCardClarification        LarkCardKind = "clarification"
	LarkCardCommentMention       LarkCardKind = "comment_mention"
)

// SupportedLarkEvents is the closed set of multica events the Lark
// router considers. Adding a row here is itself a golden-file diff
// (lark_routing_test.go iterates this list and asserts the matrix
// covers every supported event), which makes scope expansion visible
// in code review.
//
// Per §14.3: chat / github / daemon / project / reaction events are
// deliberately out — they don't have a Lark routing semantics in the
// design and adding them should not be silently allowed.
var SupportedLarkEvents = []string{
	protocol.EventIssueCreated,
	protocol.EventIssueUpdated,
	protocol.EventCommentCreated,
	protocol.EventTaskCompleted,
	protocol.EventTaskFailed,
	protocol.EventTaskDispatch,
	protocol.EventTaskProgress,
	// Meeting events are intentionally absent — meeting:created is a
	// P7 deliverable and its routing case will be added (with a
	// matching golden row) in the same PR that lands the protocol
	// constant. Keeping the set closed prevents silent inclusion.
}

// IsLarkRoutableEvent reports whether an event kind is in the closed
// set above. Callers that subscribe to the bus use this as a guard.
func IsLarkRoutableEvent(kind string) bool {
	for _, e := range SupportedLarkEvents {
		if e == kind {
			return true
		}
	}
	return false
}

// LarkUserPref is the per-user subscription state for personal-mode
// signals. Per §6.1 the OAuth default only enables Assigned +
// AgentClarification; everything else defaults off until the user
// opts in. The router takes the user's actual snapshot — defaults
// are applied by the caller, not here, so the routing function stays
// referentially transparent (a value with all false-fields means
// "all opted out", not "use defaults").
type LarkUserPref struct {
	// AssignedDM: receive a DM card when an issue is (re)assigned to
	// this user. Default ON.
	AssignedDM bool `yaml:"assigned_dm" json:"assigned_dm"`

	// AgentClarificationDM: receive a DM when an agent assigned to
	// this user posts a clarification comment AND the issue has no
	// lark_issue_link (no thread to reply into). Default ON.
	AgentClarificationDM bool `yaml:"agent_clarification_dm" json:"agent_clarification_dm"`

	// TaskFailedDM: receive a DM when a task this user owns / created
	// fails. Default OFF — daemon auto-retries, and continuous failures
	// escalate via clarification.
	TaskFailedDM bool `yaml:"task_failed_dm" json:"task_failed_dm"`

	// TaskCompletedDM: receive a DM when a task this user owns / created
	// completes. Default OFF — completion is usually checked via the
	// multica UI, not pushed.
	TaskCompletedDM bool `yaml:"task_completed_dm" json:"task_completed_dm"`

	// MentionDM: receive a DM when @-mentioned in a non-clarification
	// comment. Default OFF — the multica web UI inbox is the canonical
	// place for mentions.
	MentionDM bool `yaml:"mention_dm" json:"mention_dm"`
}

// DefaultLarkUserPref returns the pref a freshly-OAuthed user starts
// with. Per §6.1: only assigned + agent_clarification.
func DefaultLarkUserPref() LarkUserPref {
	return LarkUserPref{
		AssignedDM:           true,
		AgentClarificationDM: true,
		TaskCompletedDM:      true,
		TaskFailedDM:         true,
	}
}

// LarkRoutingConditions is the payload-derived input to the routing
// function. Listeners project the bus event into this shape; the
// routing function itself touches no DB and no I/O, which is what
// makes the golden test possible.
type LarkRoutingConditions struct {
	// Event is one of SupportedLarkEvents. Anything outside that set
	// yields an empty decision.
	Event string `yaml:"event"`

	// HasAssignee: the issue (or the issue this task / comment belongs
	// to) currently has a non-empty assignee_id.
	HasAssignee bool `yaml:"has_assignee"`

	// HasLarkIssueLink: lark_issue_link has a row for this issue. When
	// true, the issue came from a Lark thread and bot replies route
	// into that thread instead of the team chat.
	HasLarkIssueLink bool `yaml:"has_lark_issue_link"`

	// AssigneeChanged: applies to issue:updated only. The §6.1 rule is
	// "only assignee_changed=true emits a card; other updates default
	// silent" — golden cases assert both branches.
	AssigneeChanged bool `yaml:"assignee_changed"`

	// CommentType: applies to comment:created only. Empty defaults to
	// the regular "comment" type. The clarification routing branches
	// off type='clarification'.
	CommentType string `yaml:"comment_type"`

	// HasMention: applies to comment:created only. True when the
	// comment text references a member / agent / squad / @all via
	// the multica mention syntax — used to gate non-clarification
	// comments out of DM by default.
	HasMention bool `yaml:"has_mention"`

	// UserPref is the assignee's (or mentioned user's) DM preference
	// snapshot. The caller must apply DefaultLarkUserPref() for users
	// who have not yet linked their account.
	UserPref LarkUserPref `yaml:"user_pref"`

	// AssigneeIsWorkspaceAgent is true when the assignee is a workspace-
	// level (cloud) agent rather than a member or a local (daemon) agent.
	// Workspace agents have no personal owner to DM — the assignment is
	// a team-visible signal, so it routes to team chat instead of DM.
	// Local agents route to the owner's DM (the listener resolves the
	// owner's user ID as the assigneeUserID).
	AssigneeIsWorkspaceAgent bool `yaml:"assignee_is_workspace_agent"`

	// HasActiveLarkMessageRef applies to streaming patch events
	// (task:dispatch, task:progress). True when lark_message_ref
	// already holds a card for this task / issue stream — i.e. an
	// earlier signal (typically a *_proposed comment or the dispatch
	// event itself) opened a card that subsequent patches mutate.
	//
	// Without an active ref there is nothing to patch, so the
	// routing function returns a silent decision for those events
	// even when HasAssignee / HasLarkIssueLink would otherwise pick
	// a channel. This guards against the streaming patch path
	// accidentally pushing every normal task execution into Lark
	// once P2/P3 wiring lands.
	HasActiveLarkMessageRef bool `yaml:"has_active_lark_message_ref"`
}

// LarkRoutingDecision is what the function returns. Channels is the
// (possibly empty) list of destinations; the listener fans out the
// card to each. Card identifies the template — empty when Channels
// is empty.
//
// MustNotChannels is computed by the routing function: it lists every
// LarkChannel value that is NOT in Channels. The golden test asserts
// against MustNotChannels too, so "route accidentally fans out to
// both team AND dm" is caught as loudly as "route picked the wrong
// single channel".
type LarkRoutingDecision struct {
	Channels        []LarkChannel `yaml:"channels"`
	Card            LarkCardKind  `yaml:"card,omitempty"`
	MustNotChannels []LarkChannel `yaml:"must_not_channels,omitempty"`
}

// allChannels is the order MustNotChannels enumerates in. Kept
// deterministic so golden YAML diffs don't flicker on rerun.
var allChannels = []LarkChannel{
	LarkChannelTeam,
	LarkChannelDM,
	LarkChannelThreadReply,
}

// RouteLarkEvent is the pure decision function. It encodes the §6.1
// routing table. The implementation here is deliberately verbose
// (one switch arm per event, explicit branches per condition) — this
// is the file PR reviewers will inspect when scope changes, so
// readability beats cleverness.
//
// Conditions outside an event's relevance (e.g. CommentType for a
// task event) are ignored.
func RouteLarkEvent(c LarkRoutingConditions) LarkRoutingDecision {
	var channels []LarkChannel
	var card LarkCardKind

	switch c.Event {
	case protocol.EventIssueCreated:
		// Routing fan-out is additive: a thread anchor and an assignee
		// can both apply to the same issue (someone @bot-creates an
		// issue and immediately assigns it). The thread gets one
		// "created" confirmation; the assignee gets the personal card.
		// The unassigned-no-thread case is what triggers the team
		// claim card.
		if c.HasLarkIssueLink {
			channels = append(channels, LarkChannelThreadReply)
			card = LarkCardCreatedConfirmation
		}
		if c.HasAssignee {
			if c.AssigneeIsWorkspaceAgent {
				channels = append(channels, LarkChannelTeam)
			} else if c.UserPref.AssignedDM {
				channels = append(channels, LarkChannelDM)
			}
			if card == LarkCardNone {
				card = LarkCardAssigned
			}
		}
		if !c.HasAssignee && !c.HasLarkIssueLink {
			channels = append(channels, LarkChannelTeam)
			card = LarkCardClaim
		}

	case protocol.EventIssueUpdated:
		// §6.1: only assignee_changed=true emits a card; everything
		// else is silent. assignee_changed without a new assignee is
		// an unassignment — also silent (no one to DM).
		if c.AssigneeChanged && c.HasAssignee {
			if c.AssigneeIsWorkspaceAgent {
				channels = append(channels, LarkChannelTeam)
			} else if c.UserPref.AssignedDM {
				channels = append(channels, LarkChannelDM)
			}
			card = LarkCardAssigned
		}

	case protocol.EventTaskCompleted:
		// §6.1: completed → dm only, default OFF. No team, no thread.
		if c.UserPref.TaskCompletedDM {
			channels = append(channels, LarkChannelDM)
			card = LarkCardTaskCompleted
		}

	case protocol.EventTaskFailed:
		// Public-blocker carve-out (§6.1): the ONLY condition that
		// promotes a failure into the team chat is "no assignee +
		// failed". Anything else stays personal.
		if !c.HasAssignee {
			channels = append(channels, LarkChannelTeam)
			card = LarkCardPublicBlocker
			break
		}
		// Personal failure: default OFF, opt-in via pref.
		if c.UserPref.TaskFailedDM {
			channels = append(channels, LarkChannelDM)
			card = LarkCardTaskFailedPersonal
		}

	case protocol.EventCommentCreated:
		// Clarification is the §6.6 bridge: thread when the issue has
		// one, DM otherwise. The DM path is gated on the assignee's
		// AgentClarificationDM pref (default ON).
		if c.CommentType == "clarification" {
			if c.HasLarkIssueLink {
				channels = append(channels, LarkChannelThreadReply)
				card = LarkCardClarification
				break
			}
			if c.UserPref.AgentClarificationDM {
				channels = append(channels, LarkChannelDM)
				card = LarkCardClarification
			}
			break
		}
		// Non-clarification comments only matter to the recipient
		// when they mention someone. Even then, the design defaults
		// MentionDM OFF — the inbox is the canonical mention surface.
		if c.HasMention && c.UserPref.MentionDM {
			channels = append(channels, LarkChannelDM)
			card = LarkCardCommentMention
		}

	case protocol.EventTaskDispatch, protocol.EventTaskProgress:
		// §14.1.1 placeholder / patch cards travel with the issue:
		// thread when linked, otherwise DM to the assignee. Two
		// invariants the golden pins:
		//   (1) Without either anchor (no thread + no assignee)
		//       there's no card location, decision is silent.
		//   (2) Without an active lark_message_ref, there is no card
		//       to patch — emitting the patch event would either no-op
		//       (best case) or, when the wiring later treats the
		//       patch as a "create-then-patch" fallback, push every
		//       normal task execution into Lark. Silent in that case
		//       too; the *_proposed comment is what opens the card.
		if !c.HasActiveLarkMessageRef {
			break
		}
		if c.HasLarkIssueLink {
			channels = append(channels, LarkChannelThreadReply)
		} else if c.HasAssignee {
			channels = append(channels, LarkChannelDM)
		}
		// Card kind is intentionally unset for streaming events —
		// they patch an existing card whose kind was decided by the
		// originating *_proposed comment. The golden asserts the
		// channels but not a card kind for these.
	}

	canonical := canonicalChannels(channels)
	return LarkRoutingDecision{
		Channels:        canonical,
		Card:            card,
		MustNotChannels: complementChannels(canonical),
	}
}

// canonicalChannels reorders the router's appended channels into
// allChannels order. Without this the slice order would depend on
// the switch arms' append sequence (e.g. thread before dm in
// issue:created when both anchors apply), which is irrelevant
// behaviour but would force golden authors to mirror the source
// order. Sorting collapses that detail.
func canonicalChannels(picked []LarkChannel) []LarkChannel {
	if len(picked) == 0 {
		return nil
	}
	seen := make(map[LarkChannel]struct{}, len(picked))
	for _, c := range picked {
		seen[c] = struct{}{}
	}
	out := make([]LarkChannel, 0, len(picked))
	for _, c := range allChannels {
		if _, ok := seen[c]; ok {
			out = append(out, c)
		}
	}
	return out
}

// complementChannels returns allChannels \ picked, preserving the
// allChannels order so the golden YAML is stable. Used as the
// "negative space" assertion in golden cases: a regression that
// adds an extra channel to a previously-silent event triggers a
// MustNotChannels mismatch even if Channels itself is a superset.
func complementChannels(picked []LarkChannel) []LarkChannel {
	if len(picked) == 0 {
		// All channels are off-limits when the decision is silent.
		// Returning the full list (rather than nil) lets the golden
		// for "issue:updated, assignee_changed=false" explicitly
		// assert "must not go to any channel".
		out := make([]LarkChannel, len(allChannels))
		copy(out, allChannels)
		return out
	}
	seen := make(map[LarkChannel]struct{}, len(picked))
	for _, c := range picked {
		seen[c] = struct{}{}
	}
	out := make([]LarkChannel, 0, len(allChannels)-len(picked))
	for _, c := range allChannels {
		if _, ok := seen[c]; !ok {
			out = append(out, c)
		}
	}
	return out
}
