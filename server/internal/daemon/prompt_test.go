package daemon

import (
	"strings"
	"testing"
)

// TestBuildQuickCreatePromptRules locks in the rules that govern how the
// quick-create agent is allowed to translate raw user input into the issue
// description body. Each substring corresponds to a concrete failure mode
// observed in production output:
//   - meta-instructions ("create an issue", "cc @X") leaking into the body
//   - the Context section being misused as an apology log when no external
//     references were actually fetched
//   - hard-line rules being silently dropped on prompt rewrites
func TestBuildQuickCreatePromptRules(t *testing.T) {
	out := buildQuickCreatePrompt(Task{QuickCreatePrompt: "fix the login button color"})

	mustContain := []string{
		// high-fidelity invariant
		"Faithfully restate what the user wants",
		"Preserve specific names, identifiers, file paths",
		// strip non-spec material: verbal routing wrappers + conversational fillers
		"verbal routing wrappers about creating the issue",
		"pure conversational fillers",
		// cc routing must survive: mention link stays in description so the
		// auto-subscribe path fires (multica issue create has no --subscriber flag)
		"CC exception",
		"auto-subscribes members",
		// context section is conditional and must not be an apology log
		"include ONLY when the input cited external resources",
		"never use it as an apology log",
		// output/reporting must be workspace-prefix agnostic. Workspaces can
		// use custom issue prefixes, so a successful issue creation should
		// not look failed merely because the identifier does not match one
		// fixed prefix.
		"multica issue create --output json",
		"JSON response",
		"identifier",
		"Do not scrape human output",
		"do not assume any workspace issue prefix",
		"Created <identifier-or-id>: <title>",
		// hard rules
		"never invent requirements",
		"never reduce multi-sentence input",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("buildQuickCreatePrompt output missing required rule: %q", s)
		}
	}
}

// TestBuildQuickCreatePromptAssigneeIncludesSquads locks in the MUL-2165
// fix: the assignee-resolution rules must tell the agent to consult the
// squad list alongside members and agents. Before this, a quick-create
// input like "assign to <SquadName>" silently fell through to
// "Unrecognized assignee" because squads were never queried.
func TestBuildQuickCreatePromptAssigneeIncludesSquads(t *testing.T) {
	out := buildQuickCreatePrompt(Task{QuickCreatePrompt: "fix the login button color"})
	mustContain := []string{
		"multica squad list",
		"Squads are first-class assignees",
		"Treat bare @-routing as an assignee directive",
		"让 @独立团 review 这个 PR",
		"pass the squad's `id` as `--assignee-id`",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("buildQuickCreatePrompt assignee block missing %q\n--- output ---\n%s", s, out)
		}
	}
}

// TestBuildQuickCreatePromptSquadDefaultsToSquad locks in the MUL-2203
// fix: when the picker was a squad, the task runs on the squad's leader
// agent, but the default assignee for issues created by this run must
// point at the SQUAD's UUID — not the leader agent's UUID. The previous
// "default to YOURSELF" instruction made squad-created issues land under
// the leader, hiding them from the squad's delegation flow.
func TestBuildQuickCreatePromptSquadDefaultsToSquad(t *testing.T) {
	const (
		squadID   = "aaaa1111-2222-3333-4444-555555555555"
		squadName = "独立团"
		leaderID  = "bbbb1111-2222-3333-4444-666666666666"
	)
	out := buildQuickCreatePrompt(Task{
		QuickCreatePrompt: "fix the login button color",
		Agent:             &AgentData{ID: leaderID, Name: "leader-agent"},
		SquadID:           squadID,
		SquadName:         squadName,
	})

	// The default-assignee instruction must point at the squad UUID.
	if !strings.Contains(out, "--assignee-id \""+squadID+"\"") {
		t.Errorf("buildQuickCreatePrompt with SquadID must default to the squad's UUID, got:\n%s", out)
	}
	// And it must NOT tell the agent to default to itself (the leader).
	if strings.Contains(out, "--assignee-id \""+leaderID+"\"") {
		t.Errorf("buildQuickCreatePrompt with SquadID must NOT default to the leader agent's UUID, got:\n%s", out)
	}
	// The squad name should appear in the instruction so the agent has
	// human-readable context for the routing decision.
	if !strings.Contains(out, squadName) {
		t.Errorf("buildQuickCreatePrompt with SquadID should mention the squad name %q, got:\n%s", squadName, out)
	}
	// And the prompt must explicitly call out the squad-vs-leader rule
	// so the agent does not silently regress to "default to YOURSELF".
	mustContain := []string{
		"picker SQUAD",
		"running on the squad's behalf",
		"do not assign it to your own agent UUID",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("buildQuickCreatePrompt with SquadID missing %q\n--- output ---\n%s", s, out)
		}
	}
}

// TestBuildQuickCreatePromptProjectPinning verifies that when the user
// pins a project in the quick-create modal, the prompt instructs the agent
// to pass `--project <uuid>` exactly. Without this, the agent would re-read
// the workspace default and silently drop the user's selection — the same
// "I have to retype 'in project X' every time" failure mode the modal
// addition was meant to fix.
func TestBuildQuickCreatePromptProjectPinning(t *testing.T) {
	const projectID = "11111111-2222-3333-4444-555555555555"
	out := buildQuickCreatePrompt(Task{
		QuickCreatePrompt: "fix the login button color",
		ProjectID:         projectID,
		ProjectTitle:      "Web App",
	})
	mustContain := []string{
		"--project \"" + projectID + "\"",
		"Web App",
		"modal selection is authoritative",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("buildQuickCreatePrompt with project missing %q\n--- output ---\n%s", s, out)
		}
	}

	// Without a project, the prompt must keep the legacy "omit" instruction
	// so the agent doesn't accidentally start passing --project on plain
	// quick-create runs.
	plain := buildQuickCreatePrompt(Task{QuickCreatePrompt: "fix the login button color"})
	if !strings.Contains(plain, "**project**: omit") {
		t.Errorf("buildQuickCreatePrompt without project must keep the omit instruction, got:\n%s", plain)
	}
	if strings.Contains(plain, "--project") {
		t.Errorf("buildQuickCreatePrompt without project must NOT mention --project, got:\n%s", plain)
	}
}

// TestBuildPromptSquadLeaderNoActionForMemberTrigger verifies that the
// squad leader no_action prohibition is injected in the per-turn prompt
// regardless of whether the triggering comment was posted by an agent or
// a member. This was the root cause of the "LGTM is a pure acknowledgment
// — no reply needed. Exiting silently." noise comment: the prohibition
// only fired for agent-triggered comments, so member-triggered ones
// (like "LGTM") bypassed it.
func TestBuildPromptSquadLeaderNoActionForMemberTrigger(t *testing.T) {
	task := Task{
		IssueID:               "issue-123",
		TriggerCommentID:      "comment-456",
		TriggerCommentContent: "LGTM",
		TriggerAuthorType:     "member",
		TriggerAuthorName:     "Bohan",
		Agent: &AgentData{
			Instructions: "Some instructions\n\n## Squad Operating Protocol\n\nYou are the LEADER...",
		},
	}
	out := BuildPrompt(task, "claude")
	if !strings.Contains(out, "Squad leader no_action rule") {
		t.Errorf("buildCommentPrompt must inject squad leader no_action rule for member-triggered comments, got:\n%s", out)
	}
	if !strings.Contains(out, "DO NOT post any comment") {
		t.Errorf("buildCommentPrompt must contain DO NOT post prohibition for member-triggered squad leader, got:\n%s", out)
	}
}

// TestBuildPromptSquadLeaderNoActionForAgentTrigger verifies the rule also
// fires for agent-triggered comments (the original path that already worked).
func TestBuildPromptSquadLeaderNoActionForAgentTrigger(t *testing.T) {
	task := Task{
		IssueID:               "issue-123",
		TriggerCommentID:      "comment-456",
		TriggerCommentContent: "Deploy complete.",
		TriggerAuthorType:     "agent",
		TriggerAuthorName:     "deploy-boy",
		Agent: &AgentData{
			Instructions: "Some instructions\n\n## Squad Operating Protocol\n\nYou are the LEADER...",
		},
	}
	out := BuildPrompt(task, "claude")
	if !strings.Contains(out, "Squad leader no_action rule") {
		t.Errorf("buildCommentPrompt must inject squad leader no_action rule for agent-triggered comments, got:\n%s", out)
	}
}

// TestBuildPromptCommentTriggerPromotesThreadReads pins MUL-2387 + MUL-2421:
// the per-turn prompt for a comment-triggered task must default the trigger
// thread read to `--thread <id> --tail 30` (so long threads don't dump
// hundreds of replies into the agent's context) and explain reply-cursor
// pagination for older replies. --recent N stays as the cross-thread
// fallback. Locking this in test stops the guidance from decaying back to
// either the legacy full-flat-dump or the unbounded `--thread` recipe.
func TestBuildPromptCommentTriggerPromotesThreadReads(t *testing.T) {
	const (
		issueID   = "issue-thread-1"
		triggerID = "trigger-comment-1"
	)
	task := Task{
		IssueID:               issueID,
		TriggerCommentID:      triggerID,
		TriggerCommentContent: "anything",
		TriggerAuthorType:     "member",
		TriggerAuthorName:     "Bohan",
	}
	out := BuildPrompt(task, "claude")

	mustContain := []string{
		"--thread " + triggerID,
		"--tail 30",
		"`multica issue comment list " + issueID + " --thread " + triggerID + " --tail 30 --output json`",
		"Next reply cursor:",
		"--before-id <reply-id>",
		"--recent 20 --output json",
		"Next thread cursor",
		"--before",
		"--before-id",
		"--since",
		"may combine with `--thread --tail` or `--recent`",
		"Avoid the unfiltered",
		"wastes context",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("buildCommentPrompt missing thread-first guidance %q\n--- output ---\n%s", s, out)
		}
	}

	if strings.Contains(out, "returns all comments for the issue (server caps at 2000)") {
		t.Errorf("buildCommentPrompt still carries the legacy full-dump phrasing")
	}
	if strings.Contains(out, "--thread "+triggerID+" --output json") {
		t.Errorf("buildCommentPrompt regressed to unbounded --thread recipe (no --tail) — long threads will overflow context\n--- output ---\n%s", out)
	}
}

// TestBuildPromptDefaultMentionsRecent pins that the catch-all fallback
// prompt (no trigger comment, no chat, no autopilot, no quick-create) also
// teaches the agent about --recent as the long-issue-friendly alternative
// to the flat dump.
func TestBuildPromptDefaultMentionsRecent(t *testing.T) {
	out := BuildPrompt(Task{IssueID: "issue-default-1"}, "claude")
	for _, s := range []string{
		"--recent 20 --output json",
		"Next thread cursor:",
		"--since",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("default BuildPrompt missing %q\n--- output ---\n%s", s, out)
		}
	}
	if strings.Contains(out, "--thread") {
		t.Errorf("default BuildPrompt should NOT mention --thread (no trigger comment to anchor on)\n--- output ---\n%s", out)
	}
	if strings.Contains(out, "If you need comment history") {
		t.Errorf("default BuildPrompt still carries the legacy 'If you need' soft phrasing that conflicts with the mandatory workflow\n--- output ---\n%s", out)
	}
}

// TestBuildPromptLinkedDocs_DefaultBranch verifies that Lark doc bodies
// expanded server-side at claim time are embedded into the default issue
// prompt.
func TestBuildPromptLinkedDocs_DefaultBranch(t *testing.T) {
	task := Task{
		IssueID: "issue-123",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/docx/Abc", Content: "Spec section A"},
			{URL: "https://acme.feishu.cn/docx/Missing", Error: "not_found"},
		},
	}
	out := BuildPrompt(task, "claude")
	for _, want := range []string{
		"## Linked Lark documents",
		"https://acme.feishu.cn/docx/Abc",
		"Spec section A",
		"https://acme.feishu.cn/docx/Missing",
		"[doc unavailable: not_found]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default prompt missing %q\n---\n%s", want, out)
		}
	}
}

// TestBuildPromptLinkedDocs_CommentBranch verifies the same docs surface
// on comment-triggered prompts (which take a different code path).
func TestBuildPromptLinkedDocs_CommentBranch(t *testing.T) {
	task := Task{
		IssueID:               "issue-123",
		TriggerCommentID:      "comment-456",
		TriggerCommentContent: "Have a look",
		TriggerAuthorType:     "member",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/docx/Abc", Content: "Spec body"},
		},
	}
	out := BuildPrompt(task, "claude")
	if !strings.Contains(out, "## Linked Lark documents") || !strings.Contains(out, "Spec body") {
		t.Fatalf("comment prompt missing linked-docs section:\n%s", out)
	}
}

// TestBuildPromptLinkedDocs_EmptyIsByteIdentical verifies that tasks with
// no linked docs produce exactly the prompt they did before the LinkedDocs
// field existed.
func TestBuildPromptLinkedDocs_EmptyIsByteIdentical(t *testing.T) {
	task := Task{IssueID: "issue-123"}
	out := BuildPrompt(task, "claude")
	if strings.Contains(out, "Linked Lark documents") {
		t.Fatalf("default prompt must not include linked-docs heading when LinkedDocs is empty:\n%s", out)
	}
}

// TestBuildPromptLinkedDocs_ForbiddenError pins the placeholder shape for
// the "forbidden" failure reason. The vocabulary lives in service.LinkedDoc
// (forbidden / not_found / unavailable); the prompt builder renders each
// the same way so the agent has a stable signal it can reason about.
// Drifting the placeholder format silently degrades the agent's ability to
// explain why a referenced doc is missing context.
func TestBuildPromptLinkedDocs_ForbiddenError(t *testing.T) {
	out := BuildPrompt(Task{
		IssueID: "issue-1",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/docx/Locked", Error: "forbidden"},
		},
	}, "claude")
	for _, want := range []string{
		"https://acme.feishu.cn/docx/Locked",
		"[doc unavailable: forbidden]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("forbidden doc prompt missing %q\n---\n%s", want, out)
		}
	}
}

// TestBuildPromptLinkedDocs_UnavailableError pins the "unavailable" branch
// (used for integration disabled, unsupported URL kinds like sheets/base, or
// transient API failures). Same placeholder format as forbidden / not_found.
func TestBuildPromptLinkedDocs_UnavailableError(t *testing.T) {
	out := BuildPrompt(Task{
		IssueID: "issue-1",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/sheets/SheetXyz", Error: "unavailable"},
		},
	}, "claude")
	if !strings.Contains(out, "[doc unavailable: unavailable]") {
		t.Errorf("unavailable doc prompt missing placeholder\n---\n%s", out)
	}
}

// TestBuildPromptLinkedDocs_EmptyContent verifies the third placeholder
// path: success fetch but empty body (a real Lark scenario for newly
// created blank docs). The prompt must render "[doc is empty]" rather than
// silently emit nothing — the agent still needs to acknowledge the URL was
// referenced.
func TestBuildPromptLinkedDocs_EmptyContent(t *testing.T) {
	out := BuildPrompt(Task{
		IssueID: "issue-1",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/docx/Blank", Content: ""},
		},
	}, "claude")
	for _, want := range []string{
		"https://acme.feishu.cn/docx/Blank",
		"[doc is empty]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("empty-content doc prompt missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "[doc unavailable") {
		t.Errorf("empty-content doc should not surface as unavailable\n---\n%s", out)
	}
}

// TestBuildPromptLinkedDocs_MixedSuccessAndFailure verifies that a slice
// containing every render branch — populated content, empty content,
// forbidden, not_found, unavailable — produces one well-formed section per
// doc in input order. Order preservation matters: the URLs come from
// ExtractDocURLs which is order-of-first-appearance, and the agent reasons
// about them positionally ("the spec doc is the first one referenced").
func TestBuildPromptLinkedDocs_MixedSuccessAndFailure(t *testing.T) {
	docs := []LinkedDoc{
		{URL: "https://acme.feishu.cn/docx/A", Content: "Body of A"},
		{URL: "https://acme.feishu.cn/docx/B", Error: "forbidden"},
		{URL: "https://acme.feishu.cn/docx/C", Content: ""},
		{URL: "https://acme.feishu.cn/wiki/D", Error: "not_found"},
		{URL: "https://acme.feishu.cn/sheets/E", Error: "unavailable"},
	}
	out := BuildPrompt(Task{IssueID: "issue-1", LinkedDocs: docs}, "claude")

	// Each URL appears exactly once with its expected body.
	for _, want := range []string{
		"### https://acme.feishu.cn/docx/A",
		"Body of A",
		"### https://acme.feishu.cn/docx/B",
		"[doc unavailable: forbidden]",
		"### https://acme.feishu.cn/docx/C",
		"[doc is empty]",
		"### https://acme.feishu.cn/wiki/D",
		"[doc unavailable: not_found]",
		"### https://acme.feishu.cn/sheets/E",
		"[doc unavailable: unavailable]",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("expected exactly one occurrence of %q, got %d\n---\n%s", want, strings.Count(out, want), out)
		}
	}

	// Order is preserved (A before B before C before D before E).
	idxA := strings.Index(out, "/docx/A")
	idxB := strings.Index(out, "/docx/B")
	idxC := strings.Index(out, "/docx/C")
	idxD := strings.Index(out, "/wiki/D")
	idxE := strings.Index(out, "/sheets/E")
	if !(idxA < idxB && idxB < idxC && idxC < idxD && idxD < idxE) {
		t.Errorf("doc order not preserved: A=%d B=%d C=%d D=%d E=%d", idxA, idxB, idxC, idxD, idxE)
	}

	// The section is terminated by `---` so downstream prompt fragments
	// (comment reply instructions, etc.) stay clearly separated.
	if !strings.Contains(out, "---") {
		t.Errorf("linked-docs section missing terminating separator\n---\n%s", out)
	}
}

// TestBuildPromptLinkedDocs_SpecialCharacters verifies that doc content
// containing markdown that overlaps with the prompt's own structure
// (level-3 headings, fenced code blocks, table rows) is embedded verbatim
// and does not corrupt the surrounding section. The risk is that a
// content body emitting "### " or "---" could be mistaken for the next
// linked-doc heading or the section terminator; in practice the agent
// reads the whole block as one chunk and tolerates that, but we still
// pin that the body survives byte-for-byte.
func TestBuildPromptLinkedDocs_SpecialCharacters(t *testing.T) {
	content := "## Inner heading\n" +
		"\n" +
		"Some prose with `inline code` and a [link](https://example.com).\n" +
		"\n" +
		"```go\n" +
		"func main() {\n" +
		"\tfmt.Println(\"hello\")\n" +
		"}\n" +
		"```\n" +
		"\n" +
		"| col1 | col2 |\n" +
		"|------|------|\n" +
		"| a    | b    |\n" +
		"\n" +
		"### A subsection that looks like a doc heading\n" +
		"\n" +
		"And a horizontal rule below:\n" +
		"\n" +
		"---\n"
	out := BuildPrompt(Task{
		IssueID: "issue-1",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/docx/Rich", Content: content},
			{URL: "https://acme.feishu.cn/docx/After", Content: "after marker"},
		},
	}, "claude")

	// Body is embedded byte-for-byte.
	if !strings.Contains(out, content) {
		t.Errorf("rich-content body not embedded verbatim\n---\n%s", out)
	}
	// Both docs still render their own URL heading.
	if !strings.Contains(out, "### https://acme.feishu.cn/docx/Rich") {
		t.Errorf("rich-content doc heading missing\n---\n%s", out)
	}
	if !strings.Contains(out, "### https://acme.feishu.cn/docx/After") {
		t.Errorf("trailing doc heading missing — rich content may have eaten it\n---\n%s", out)
	}
	// The second doc's body still renders, proving the rich content
	// didn't truncate the loop.
	if !strings.Contains(out, "after marker") {
		t.Errorf("doc after rich content missing\n---\n%s", out)
	}
}

// TestBuildPromptLinkedDocs_ContentNewlineNormalization verifies the
// trailing-newline normalization branch: appendLinkedDocs appends "\n"
// after a body that doesn't already end with one. This keeps section
// spacing predictable regardless of whether Lark returned its raw_content
// with or without a final newline.
func TestBuildPromptLinkedDocs_ContentNewlineNormalization(t *testing.T) {
	// Content without trailing newline → builder appends one + blank line.
	out := BuildPrompt(Task{
		IssueID: "issue-1",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/docx/NoNL", Content: "no trailing newline"},
			{URL: "https://acme.feishu.cn/docx/Next", Content: "next doc"},
		},
	}, "claude")
	// The body, a newline, a blank line, then the next heading.
	if !strings.Contains(out, "no trailing newline\n\n### https://acme.feishu.cn/docx/Next") {
		t.Errorf("missing newline normalization after body without trailing \\n\n---\n%s", out)
	}

	// Content with trailing newline → builder does NOT double it.
	out2 := BuildPrompt(Task{
		IssueID: "issue-1",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/docx/HasNL", Content: "has trailing newline\n"},
			{URL: "https://acme.feishu.cn/docx/Next", Content: "next doc"},
		},
	}, "claude")
	if strings.Contains(out2, "has trailing newline\n\n\n") {
		t.Errorf("body with trailing \\n caused triple-newline (double normalization)\n---\n%s", out2)
	}
	if !strings.Contains(out2, "has trailing newline\n\n### https://acme.feishu.cn/docx/Next") {
		t.Errorf("body with trailing \\n missing expected single blank-line gap to next heading\n---\n%s", out2)
	}
}

// TestBuildPromptLinkedDocs_TitleFieldDoesNotBreak verifies that a
// populated LinkedDoc.Title does not break rendering. The current
// prompt builder doesn't surface Title in the prompt — this is a
// tripwire: if a future change starts emitting Title, the test should
// be updated to lock in the new shape, not silently fall through.
func TestBuildPromptLinkedDocs_TitleFieldDoesNotBreak(t *testing.T) {
	out := BuildPrompt(Task{
		IssueID: "issue-1",
		LinkedDocs: []LinkedDoc{
			{URL: "https://acme.feishu.cn/docx/Titled", Title: "Spec: Auth Flow", Content: "body"},
		},
	}, "claude")
	for _, want := range []string{
		"https://acme.feishu.cn/docx/Titled",
		"body",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("titled doc prompt missing %q\n---\n%s", want, out)
		}
	}
}

// TestBuildPromptLinkedDocs_BoundedSliceContract pins the contract
// between the prompt builder and the server-side handler that supplies
// LinkedDocs. The handler caps the slice at service.MaxDocsPerClaim (5)
// before it ever reaches the daemon; the builder itself is unbounded and
// will faithfully render whatever it's handed. This test confirms the
// "render everything" half of that contract so a future regression that
// silently drops docs in the builder cannot ship undetected — the cap
// must remain at the call site, not the renderer.
func TestBuildPromptLinkedDocs_BoundedSliceContract(t *testing.T) {
	docs := make([]LinkedDoc, 8)
	for i := range docs {
		docs[i] = LinkedDoc{
			URL:     "https://acme.feishu.cn/docx/Doc" + string(rune('A'+i)),
			Content: "body " + string(rune('A'+i)),
		}
	}
	out := BuildPrompt(Task{IssueID: "issue-1", LinkedDocs: docs}, "claude")
	for _, d := range docs {
		if !strings.Contains(out, d.URL) {
			t.Errorf("builder dropped doc URL %q — cap must live at the handler, not the renderer\n---\n%s", d.URL, out)
		}
		if !strings.Contains(out, d.Content) {
			t.Errorf("builder dropped doc body %q\n---\n%s", d.Content, out)
		}
	}
}

// TestBuildPromptNonSquadLeaderNoRule verifies that non-squad-leader agents
// do NOT get the squad leader no_action rule injected.
func TestBuildPromptNonSquadLeaderNoRule(t *testing.T) {
	task := Task{
		IssueID:               "issue-123",
		TriggerCommentID:      "comment-456",
		TriggerCommentContent: "LGTM",
		TriggerAuthorType:     "member",
		TriggerAuthorName:     "Bohan",
		Agent: &AgentData{
			Instructions: "Some instructions without the squad marker",
		},
	}
	out := BuildPrompt(task, "claude")
	if strings.Contains(out, "Squad leader no_action rule") {
		t.Errorf("buildCommentPrompt must NOT inject squad leader no_action rule for non-squad-leader agents, got:\n%s", out)
	}
}
