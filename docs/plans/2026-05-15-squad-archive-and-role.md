# Squad Archive Dialog & Role Combobox Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace native `confirm()` archive flow with shadcn `AlertDialog` showing leader name + active issue count, and replace inline `<Input>` `RoleEditor` with a shadcn Combobox (Command + Popover) backed by existing roles in the squad.

**Architecture:** Two parallel tracks. (1) Backend adds an `active_issue_count` field to `SquadResponse`, sourced from a new `CountActiveIssuesForSquad` sqlc query that counts non-terminal issues assigned to the squad. (2) Frontend extends the `Squad` type + zod schema (defensive parse), replaces `confirm()` with `ArchiveSquadConfirmDialog`, and rewrites `RoleEditor` as a Command-inside-Popover combobox that aggregates `members.map(m => m.role).filter(Boolean)` for suggestions.

**Tech Stack:** Go (chi, sqlc, pgx), TypeScript, React, TanStack Query, shadcn (Base UI primitives), zod, Vitest, Playwright.

**Files in scope:**
- `server/pkg/db/queries/issue.sql` — new query
- `server/internal/handler/squad.go` — extend `SquadResponse`, populate `active_issue_count` in `GetSquad`
- `server/internal/handler/squad_test.go` (new or extend) — handler test
- `packages/core/types/squad.ts` — extend `Squad`
- `packages/core/api/schemas.ts` (and/or `schema.ts` usage in client) — squad zod schema + `parseWithFallback` in `getSquad`
- `packages/core/api/client.ts:1458` — `getSquad` parses the response
- `packages/views/squads/components/squad-detail-page.tsx:209` — header archive button
- `packages/views/squads/components/squad-detail-page.tsx:649-699` — `RoleEditor`
- `packages/views/squads/components/archive-squad-confirm-dialog.tsx` — new file
- `packages/views/locales/en/squads.json` and `packages/views/locales/zh-Hans/squads.json` — new keys
- `packages/views/squads/components/squad-detail-page.test.tsx` (new) — view tests

**Out of scope:** Workspace-wide role library, server-side role enum, redesign of any other inspector control.

---

## API Compatibility Notes (for plan reviewer)

- `SquadResponse.active_issue_count` is **additive**. Old desktop clients ignore unknown fields, so adding it is backwards compatible.
- The field is **only present on `GET /api/squads/{id}`**. `ListSquads`, `CreateSquad`, and `UpdateSquad` deliberately omit it (would be N+1 for list; semantically irrelevant for create/update which return the just-written row). `omitempty` on a nil `*int64` keeps the field absent from those responses.
- The TS field is declared **optional** (`active_issue_count?: number | null`) — see Task 3 rationale. This matches the wire truth: only the detail endpoint carries it. Making it required `number | null` would lie about list/create/update shapes.
- The archive dialog falls back to "any active issues" copy when the value is `undefined` or `null`, so an older server (no field) and a transient count error (null) collapse to the same safe rendering.
- All response parsing for `getSquad` goes through `parseWithFallback` per `CLAUDE.md → API Response Compatibility`. List/create/update remain raw `this.fetch` to stay consistent with the rest of the client (no precedent for wrapping them, and they don't touch the archive dialog).
- No DB migration required — the count is computed on read.

## Archive semantics decision (resolved)

Original plan counted only **non-terminal** issues in the dialog, but `TransferSquadAssignees` in `server/pkg/db/queries/squad.sql:71` reassigns **every** issue currently assigned to the squad regardless of status. Two paths considered, **picking (b)**:

- **(a) Match count to existing transfer**: count all rows including `done`/`cancelled`. Rejected: the dialog message would have to read "{N} issues, some closed" or stay vague; quietly rewriting the assignee on closed historical records is poor product behavior — a closed issue's "Assigned to" should reflect who owned it when it closed, not who happens to inherit the squad's leftovers years later.
- **(b) Restrict transfer to active issues** (chosen). Archive becomes "release the active work to the leader; leave history alone." Dialog count and SQL operate on one set — `status NOT IN ('done', 'cancelled')` — so there is no count/action mismatch to maintain. Terminal issues keep `assignee_type='squad', assignee_id=<archived squad>`; the existing squad row stays in the DB (only `archived_at` is set), so badge rendering still resolves the name and a "(archived)" suffix can be added later if product asks.

**Impact of (b):**
- `TransferSquadAssignees` SQL gains `AND status NOT IN ('done', 'cancelled')`.
- New Go test covers "done/cancelled issues stay assigned to the squad after archive".
- Dialog copy can confidently say "{leader} will take over {N} active issues" — no qualifier needed.
- Reads of closed issues whose assignee is an archived squad: no UI change required. `ListSquads` filters by `archived_at IS NULL`, so the squad won't surface in pickers, but lookup-by-id (used for badge rendering) still returns it. If any consumer surface needs to distinguish archived squads, that is a separate follow-up.

## Index assumption (resolved)

The previous draft (now removed) claimed an existing `(workspace_id, assignee_id)` index. Re-checked against `server/migrations/001_init.up.sql:168-170` — the actual issue indexes are:

```sql
CREATE INDEX idx_issue_workspace ON issue(workspace_id);
CREATE INDEX idx_issue_assignee  ON issue(assignee_type, assignee_id);
CREATE INDEX idx_issue_status    ON issue(workspace_id, status);
```

Real plan for `CountActiveIssuesForSquad`:

```
WHERE workspace_id = $1
  AND assignee_type = 'squad'
  AND assignee_id   = $2
  AND status NOT IN ('done', 'cancelled');
```

Postgres will use `idx_issue_assignee` (the `(assignee_type, assignee_id)` composite), narrowing first by `assignee_type='squad'`, then by `assignee_id=<squad uuid>`. The output of that index probe is **all issues currently or historically assigned to this one squad**. `workspace_id` and the `status NOT IN (…)` predicate are applied as a heap recheck on that small set.

**Cardinality estimate:** squad rosters are 2-10 entities (per `CLAUDE.md` project context), and squads are workspace-internal. Realistic upper bound for issues ever assigned to a single squad in a workspace: **low-hundreds**. The heap recheck over ≤ a few hundred rows is sub-millisecond — no measurable difference from a covering or partial index. Even a worst-case workspace (thousands of issues, dozens of squads) keeps each per-squad probe in the same band because the index restricts by squad first.

**Conclusion: no new index.** `idx_issue_assignee` is sufficient. If profiling later shows this query becomes hot on a large workspace, the right follow-up is:

```sql
CREATE INDEX CONCURRENTLY idx_issue_active_squad
  ON issue(assignee_id)
  WHERE assignee_type = 'squad'
    AND status NOT IN ('done', 'cancelled');
```

That is a partial index keyed on `assignee_id` alone, filtering both `assignee_type` and the terminal-status set at index time. Don't add it speculatively — the current index handles the realistic load.

---

## Task 1: Backend — `CountActiveIssuesForSquad` sqlc query

**Files:**
- Modify: `server/pkg/db/queries/issue.sql`

**Step 1: Write the failing test (Go integration test)**

In `server/internal/handler/squad_test.go` (extend existing or create), add:

```go
func TestGetSquadIncludesActiveIssueCount(t *testing.T) {
    ts := newTestServer(t)
    ws, owner := ts.createWorkspace(t)
    leader := ts.createAgent(t, ws.ID)
    squad := ts.createSquad(t, ws.ID, owner.UserID, leader.ID)

    // 2 active, 1 done, 1 cancelled — only active count should be returned.
    ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "todo")
    ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "in_progress")
    ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "done")
    ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "cancelled")

    resp := ts.getSquad(t, ws.ID, squad.ID)
    require.NotNil(t, resp.ActiveIssueCount, "GetSquad must populate active_issue_count")
    require.Equal(t, int64(2), *resp.ActiveIssueCount)
}
```

`ActiveIssueCount` is `*int64` (see Task 2). Compare `int64(2)` against the dereferenced pointer, and assert non-nil first so a future regression that drops the field surfaces as a clear failure instead of a nil-pointer panic.

**Step 2: Run and confirm it fails**

```bash
cd server && go test ./internal/handler/ -run TestGetSquadIncludesActiveIssueCount
```

Expected: FAIL — field `ActiveIssueCount` does not exist yet.

**Step 3: Add the sqlc query**

Append to `server/pkg/db/queries/issue.sql`:

```sql
-- name: CountActiveIssuesForSquad :one
-- Count issues currently assigned to a squad that are NOT in a terminal state.
SELECT COUNT(*)::bigint AS count
FROM issue
WHERE workspace_id = $1
  AND assignee_type = 'squad'
  AND assignee_id = $2
  AND status NOT IN ('done', 'cancelled');
```

Status set matches the same terminal pair used by `ChildIssueProgress` in this file — keep them in sync if either ever changes.

**Step 4: Regenerate sqlc**

```bash
make sqlc
```

Expected: no errors. `server/pkg/db/generated/issue.sql.go` now exposes `CountActiveIssuesForSquad`.

**Step 5: Tighten `TransferSquadAssignees` to active issues only**

Per the *Archive semantics decision* above, the archive transfer must operate on the same set the dialog counts. Edit `server/pkg/db/queries/squad.sql:71`:

```sql
-- name: TransferSquadAssignees :exec
-- Transfer ACTIVE issues assigned to a squad to the squad's leader agent.
-- Terminal-state issues (done, cancelled) keep their existing squad assignee
-- as historical record; the squad row is only soft-deleted via archived_at,
-- so badge rendering by ID still resolves.
UPDATE issue SET assignee_type = 'agent', assignee_id = $2, updated_at = now()
WHERE assignee_type = 'squad'
  AND assignee_id = $1
  AND status NOT IN ('done', 'cancelled');
```

Add a regression test in `server/internal/handler/squad_test.go` (alongside the count test):

```go
func TestDeleteSquadLeavesTerminalIssuesAssignedToSquad(t *testing.T) {
    ts := newTestServer(t)
    ws, owner := ts.createWorkspace(t)
    leader := ts.createAgent(t, ws.ID)
    squad := ts.createSquad(t, ws.ID, owner.UserID, leader.ID)

    active := ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "in_progress")
    done   := ts.createIssueAssignedToSquad(t, ws.ID, squad.ID, "done")

    ts.deleteSquad(t, ws.ID, squad.ID, owner.UserID)

    // active → reassigned to leader
    got := ts.getIssue(t, active.ID)
    require.Equal(t, "agent", got.AssigneeType)
    require.Equal(t, leader.ID.String(), got.AssigneeID)

    // done → still on the (now archived) squad
    got = ts.getIssue(t, done.ID)
    require.Equal(t, "squad", got.AssigneeType)
    require.Equal(t, squad.ID.String(), got.AssigneeID)
}
```

**Step 6: Regenerate sqlc and run both tests**

```bash
make sqlc
cd server && go test ./internal/handler/ -run "TestGetSquadIncludesActiveIssueCount|TestDeleteSquadLeavesTerminalIssuesAssignedToSquad" -v
```

Expected: PASS (after Task 2 wires the handler).

**Step 7: Commit**

```bash
git add server/pkg/db/queries/ server/pkg/db/generated/
git commit -m "feat(squad): count + transfer only active issues on archive"
```

---

## Task 2: Backend — extend `SquadResponse` with `active_issue_count`

**Files:**
- Modify: `server/internal/handler/squad.go:18-31` (response type) and `:193-199` (`GetSquad` handler)

**Step 1: Extend `SquadResponse`**

Add field at the bottom of the struct (after `ArchivedBy`) — keeps JSON ordering stable for any consumers that depend on it:

```go
type SquadResponse struct {
    // ... existing fields ...
    ArchivedBy       *string `json:"archived_by"`
    ActiveIssueCount *int64  `json:"active_issue_count,omitempty"`
}
```

`*int64` + `omitempty` because:
- `ListSquads` doesn't populate it (would be N+1) — must serialize as absent, not `0`.
- A pointer makes "not populated" distinguishable from "zero issues".

**Step 2: Leave `squadToResponse` unchanged**

`squadToResponse(s db.Squad)` stays returning `ActiveIssueCount: nil`. Counting per row in the list endpoint would be N+1; only `GetSquad` needs the count.

**Step 3: Populate in `GetSquad`**

Replace `squad.go:193-199`:

```go
func (h *Handler) GetSquad(w http.ResponseWriter, r *http.Request) {
    squad, _, ok := h.loadSquadInWorkspace(w, r)
    if !ok {
        return
    }
    resp := squadToResponse(squad)

    count, err := h.Queries.CountActiveIssuesForSquad(r.Context(), db.CountActiveIssuesForSquadParams{
        WorkspaceID: squad.WorkspaceID,
        AssigneeID:  uuidToPgPtr(squad.ID), // assignee_id is nullable; squad.ID is non-null
    })
    if err != nil {
        // Non-fatal: log and continue with nil. The UI degrades to "leader only" copy.
        slog.Warn("count active squad issues failed", "squad_id", uuidToString(squad.ID), "error", err)
    } else {
        resp.ActiveIssueCount = &count
    }
    writeJSON(w, http.StatusOK, resp)
}
```

> Note: verify `CountActiveIssuesForSquadParams` param shape after `make sqlc`. `assignee_id` is a nullable UUID column in `issue`, so sqlc will likely generate `pgtype.UUID` directly; if the field is `pgtype.UUID` (not a pointer), pass `squad.ID` directly. **Adjust to match generated code** — do not invent helper names.

**Step 4: Run the test from Task 1**

```bash
cd server && go test ./internal/handler/ -run TestGetSquadIncludesActiveIssueCount -v
```

Expected: PASS.

**Step 5: Run full handler test suite**

```bash
cd server && go test ./internal/handler/...
```

Expected: all pass — no regressions.

**Step 6: Commit**

```bash
git add server/internal/handler/squad.go server/internal/handler/squad_test.go
git commit -m "feat(squad): include active_issue_count in GetSquad response"
```

---

## Task 3: Frontend — extend `Squad` type + zod schema

**Files:**
- Modify: `packages/core/types/squad.ts:5-18`
- Modify: `packages/core/api/schemas.ts` (add `SquadSchema`) and `packages/core/api/client.ts:1458` (parse with fallback)

**Step 1: Extend the TS type — field is OPTIONAL**

```ts
export interface Squad {
  // ... existing fields ...
  archived_by: string | null;
  /**
   * Active (non-terminal) issues currently assigned to this squad.
   * Only present on `GET /api/squads/{id}` responses; absent (`undefined`)
   * on list/create/update responses, and absent on older servers that
   * predate the field. Treat `undefined` and `null` identically as "unknown".
   */
  active_issue_count?: number | null;
}
```

**Why optional, not required `number | null`:** the field is genuinely only emitted by `GetSquad`. Server-side `squadToResponse` in `server/internal/handler/squad.go:44-59` is the converter for *all four* Squad-returning endpoints (`ListSquads`, `CreateSquad`, `UpdateSquad`, `GetSquad`), and only `GetSquad` overrides `resp.ActiveIssueCount` after calling it. Combined with `omitempty` on a nil `*int64`, list/create/update responses do not contain the JSON key at all. Declaring the TS field as required `number | null` would lie about those three shapes — TypeScript would happily let consumers read `squad.active_issue_count` from a `listSquads()` result and get `undefined` at runtime with no warning. Optional makes the contract honest and forces callers that need the value (the archive dialog) to handle the missing case, which it already does.

**Step 2: Add `SquadSchema` in `schemas.ts` (with `.loose()` per local convention)**

```ts
export const SquadSchema = z.object({
  id: z.string(),
  workspace_id: z.string(),
  name: z.string().catch(""),
  description: z.string().catch(""),
  instructions: z.string().catch(""),
  avatar_url: z.string().nullable().catch(null),
  leader_id: z.string(),
  creator_id: z.string(),
  created_at: z.string(),
  updated_at: z.string(),
  archived_at: z.string().nullable().catch(null),
  archived_by: z.string().nullable().catch(null),
  active_issue_count: z.number().nullable().optional().catch(null),
}).loose();
```

Two convention points enforced here:
1. `.loose()` follows `packages/core/api/schemas.ts:25-31` — without it, zod 4 would silently strip any unknown server-side field a future PR adds; with it, unknown fields pass through unchanged.
2. `active_issue_count` is `.nullable().optional().catch(null)` — accepts `number`, `null`, or **missing**, and falls back to `null` on any malformed value. This is what makes the schema compatible with all four endpoint shapes: missing on list/create/update, present-with-number on `GetSquad`, present-with-null when the server's count query errors.

**Schema/normalization scope:** wrap **only `getSquad`** with `parseWithFallback` (Step 3). `listSquads`/`createSquad`/`updateSquad` stay as raw `this.fetch` — they are not consumed by any code that reads `active_issue_count`, and wrapping them now would create a precedent (no other list/create/update method in `client.ts` is schema-wrapped) without a concrete defensive payoff. If we later need defensive parsing for list, we add it then.

**Step 3: Use `parseWithFallback` in `getSquad`**

In `packages/core/api/client.ts:1458`:

```ts
async getSquad(id: string): Promise<Squad> {
  const raw = await this.fetch(`/api/squads/${id}`);
  return parseWithFallback(raw, SquadSchema, raw as Squad, {
    endpoint: `GET /api/squads/${id}`,
  });
}
```

**Step 4: Add a schema test**

In `packages/core/api/schema.test.ts`, add:

```ts
const baseSquad = {
  id: "x", workspace_id: "y", name: "n", description: "", instructions: "",
  avatar_url: null, leader_id: "l", creator_id: "c", created_at: "t",
  updated_at: "t", archived_at: null, archived_by: null,
};

it("SquadSchema accepts a response missing active_issue_count (old server / list endpoint)", () => {
  const result = parseWithFallback(baseSquad, SquadSchema, null as never, { endpoint: "test" });
  // Field is optional — accept either undefined or null on read; both mean "unknown".
  expect(result.active_issue_count ?? null).toBeNull();
});

it("SquadSchema parses a response with active_issue_count: number", () => {
  const result = parseWithFallback(
    { ...baseSquad, active_issue_count: 3 },
    SquadSchema, null as never, { endpoint: "test" }
  );
  expect(result.active_issue_count).toBe(3);
});

it("SquadSchema parses active_issue_count: null (server-side count error path)", () => {
  const result = parseWithFallback(
    { ...baseSquad, active_issue_count: null },
    SquadSchema, null as never, { endpoint: "test" }
  );
  expect(result.active_issue_count).toBeNull();
});

it("SquadSchema preserves unknown fields via .loose()", () => {
  const result = parseWithFallback(
    { ...baseSquad, future_field: "x" },
    SquadSchema, null as never, { endpoint: "test" }
  );
  expect((result as Record<string, unknown>).future_field).toBe("x");
});
```

**Step 5: Run and confirm green**

```bash
pnpm --filter @multica/core exec vitest run api/schema.test.ts
```

Expected: PASS.

**Step 6: Typecheck**

```bash
pnpm typecheck
```

Expected: PASS — including `packages/views/squads/components/squad-detail-page.tsx` (existing usage of `Squad` does not destructure `active_issue_count`).

**Step 7: Commit**

```bash
git add packages/core/types/squad.ts packages/core/api/schemas.ts packages/core/api/client.ts packages/core/api/schema.test.ts
git commit -m "feat(squad): parse active_issue_count via SquadSchema in getSquad"
```

---

## Task 4: i18n — add `squads.archive_dialog.*` keys

**Files:**
- Modify: `packages/views/locales/en/squads.json`
- Modify: `packages/views/locales/zh-Hans/squads.json`

**Step 1: English keys**

Add a top-level `"archive_dialog"` block to `en/squads.json`:

```json
"archive_dialog": {
  "title": "Archive squad \"{{name}}\"?",
  "description_with_count_one": "{{leader}} will take over {{count}} active issue. The squad will no longer appear in the workspace list, and this cannot be undone.",
  "description_with_count_other": "{{leader}} will take over {{count}} active issues. The squad will no longer appear in the workspace list, and this cannot be undone.",
  "description_no_count": "{{leader}} will take over any active issues for this squad. The squad will no longer appear in the workspace list, and this cannot be undone.",
  "cancel": "Cancel",
  "confirm": "Archive",
  "archiving": "Archiving…"
}
```

**Step 2: Chinese keys** (`zh-Hans/squads.json`)

```json
"archive_dialog": {
  "title": "归档小队「{{name}}」?",
  "description_with_count_one": "{{leader}} 将接管 {{count}} 个进行中的 issue。归档后小队不再出现在工作区列表中,该操作不可撤销。",
  "description_with_count_other": "{{leader}} 将接管 {{count}} 个进行中的 issue。归档后小队不再出现在工作区列表中,该操作不可撤销。",
  "description_no_count": "{{leader}} 将接管该小队所有进行中的 issue。归档后小队不再出现在工作区列表中,该操作不可撤销。",
  "cancel": "取消",
  "confirm": "归档",
  "archiving": "归档中…"
}
```

**Step 3: Add Role combobox keys**

Add to both locales under a new `"role_editor"` block:

```json
// en
"role_editor": {
  "add_role": "+ Add role",
  "search_placeholder": "Type or pick a role…",
  "no_suggestions": "No existing roles. Press Enter to add."
}
// zh-Hans
"role_editor": {
  "add_role": "+ 添加角色",
  "search_placeholder": "输入或选择角色…",
  "no_suggestions": "尚无已用角色,按 Enter 添加"
}
```

**Step 4: Typecheck (i18n key types are generated)**

```bash
pnpm typecheck
```

Expected: PASS — locale type generation picks up new keys automatically (see `packages/views/i18n/`).

**Step 5: Commit**

```bash
git add packages/views/locales/
git commit -m "feat(squad): add i18n keys for archive dialog and role editor"
```

---

## Task 5: Frontend — `ArchiveSquadConfirmDialog`

**Files:**
- Create: `packages/views/squads/components/archive-squad-confirm-dialog.tsx`
- Test: `packages/views/squads/components/archive-squad-confirm-dialog.test.tsx`

**Step 1: Write the failing test**

```tsx
// archive-squad-confirm-dialog.test.tsx
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { ArchiveSquadConfirmDialog } from "./archive-squad-confirm-dialog";

describe("ArchiveSquadConfirmDialog", () => {
  it("shows leader name and count when count is provided", () => {
    render(
      <ArchiveSquadConfirmDialog
        open
        squadName="Squirtle"
        leaderName="Squirtle-Leader"
        activeIssueCount={3}
        onCancel={() => {}}
        onConfirm={async () => {}}
        pending={false}
      />
    );
    expect(screen.getByText(/Squirtle-Leader/)).toBeInTheDocument();
    expect(screen.getByText(/3/)).toBeInTheDocument();
  });

  it("falls back to no-count copy when activeIssueCount is null", () => {
    render(
      <ArchiveSquadConfirmDialog
        open squadName="S" leaderName="L"
        activeIssueCount={null}
        onCancel={() => {}} onConfirm={async () => {}}
        pending={false}
      />
    );
    expect(screen.getByText(/any active issues/i)).toBeInTheDocument();
  });

  it("disables confirm and cancel while pending", () => {
    render(
      <ArchiveSquadConfirmDialog
        open squadName="S" leaderName="L"
        activeIssueCount={1}
        onCancel={() => {}} onConfirm={async () => {}}
        pending
      />
    );
    expect(screen.getByRole("button", { name: /archiving/i })).toBeDisabled();
  });

  it("calls onConfirm when user clicks Archive", async () => {
    const onConfirm = vi.fn().mockResolvedValue(undefined);
    render(
      <ArchiveSquadConfirmDialog
        open squadName="S" leaderName="L"
        activeIssueCount={1}
        onCancel={() => {}} onConfirm={onConfirm}
        pending={false}
      />
    );
    await userEvent.click(screen.getByRole("button", { name: /^archive$/i }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });
});
```

**Step 2: Run — verify it fails**

```bash
pnpm --filter @multica/views exec vitest run squads/components/archive-squad-confirm-dialog.test.tsx
```

Expected: FAIL — module not found.

**Step 3: Implement the dialog**

```tsx
"use client";

import { Loader2 } from "lucide-react";
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useT } from "../../i18n";

export function ArchiveSquadConfirmDialog({
  open, squadName, leaderName, activeIssueCount,
  onCancel, onConfirm, pending,
}: {
  open: boolean;
  squadName: string;
  leaderName: string;
  activeIssueCount: number | null;
  onCancel: () => void;
  onConfirm: () => Promise<void> | void;
  pending: boolean;
}) {
  const { t } = useT("squads");
  const description =
    activeIssueCount == null
      ? t(($) => $.archive_dialog.description_no_count, { leader: leaderName })
      : t(
          ($) => activeIssueCount === 1
            ? $.archive_dialog.description_with_count_one
            : $.archive_dialog.description_with_count_other,
          { leader: leaderName, count: activeIssueCount }
        );

  return (
    <AlertDialog open={open} onOpenChange={(v) => { if (!v && !pending) onCancel(); }}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {t(($) => $.archive_dialog.title, { name: squadName })}
          </AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>
            {t(($) => $.archive_dialog.cancel)}
          </AlertDialogCancel>
          <AlertDialogAction
            onClick={() => void onConfirm()}
            disabled={pending}
            className="bg-destructive text-white hover:bg-destructive/90"
          >
            {pending ? (
              <><Loader2 className="size-3.5 mr-1 animate-spin" />{t(($) => $.archive_dialog.archiving)}</>
            ) : (
              t(($) => $.archive_dialog.confirm)
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
```

**Step 4: Verify tests pass**

```bash
pnpm --filter @multica/views exec vitest run squads/components/archive-squad-confirm-dialog.test.tsx
```

Expected: PASS.

**Step 5: Commit**

```bash
git add packages/views/squads/components/archive-squad-confirm-dialog.tsx packages/views/squads/components/archive-squad-confirm-dialog.test.tsx
git commit -m "feat(squad): add ArchiveSquadConfirmDialog component"
```

---

## Task 6: Frontend — wire the dialog into `SquadDetailPage`

**Files:**
- Modify: `packages/views/squads/components/squad-detail-page.tsx:209` (header button) and add state at top of `SquadDetailPage`

**Step 1: Add `archiveOpen` state**

Inside `SquadDetailPage()` near the other `useState` calls (e.g. near `showAddMember`):

```tsx
const [archiveOpen, setArchiveOpen] = useState(false);
```

**Step 2: Replace the header button**

Replace line 209:

```tsx
<Button
  size="sm"
  variant="ghost"
  className="text-destructive hover:text-destructive"
  onClick={() => setArchiveOpen(true)}
>
  <Trash2 className="size-3.5 mr-1" />
  {t(($) => $.inspector.archive_button)}
</Button>
```

**Step 3: Render the dialog**

Below the existing dialogs (after `showCreateAgent` block, before the closing `</div>` of the page root):

```tsx
<ArchiveSquadConfirmDialog
  open={archiveOpen}
  squadName={squad.name}
  leaderName={getEntityName("agent", squad.leader_id)}
  // `active_issue_count` is `number | null | undefined` (optional). Coerce
  // `undefined` (older server / list-shaped data) to `null` so the dialog's
  // "no count" branch covers both cases identically.
  activeIssueCount={squad.active_issue_count ?? null}
  pending={deleteMut.isPending}
  onCancel={() => setArchiveOpen(false)}
  onConfirm={async () => {
    await deleteMut.mutateAsync();
    setArchiveOpen(false);
  }}
/>
```

**Step 4: Update `deleteMut` to support `mutateAsync`**

Verify in the existing mutation block — `useMutation` always exposes `mutateAsync`. If `deleteMut` is currently typed without `await` callers, no change needed.

**Step 5: Manual verification**

```bash
make dev
```

- Navigate to a squad detail page → click `Archive` → AlertDialog opens with leader name and count
- Click `Cancel` → closes, no mutation
- Click `Archive` → button shows `Loader2`, disabled; after success, redirects to squads list (existing `deleteMut` `onSuccess` behavior)
- Open DevTools → simulate `active_issue_count: null` response (older-server scenario) → dialog shows fallback copy

**Step 6: Run typecheck + view tests**

```bash
pnpm typecheck && pnpm --filter @multica/views test
```

**Step 7: Commit**

```bash
git add packages/views/squads/components/squad-detail-page.tsx
git commit -m "feat(squad): wire ArchiveSquadConfirmDialog into squad detail header"
```

---

## Task 7: Frontend — extract `RoleEditor` into Combobox (Command + Popover)

**Files:**
- Modify: `packages/views/squads/components/squad-detail-page.tsx:649-699` (`RoleEditor` implementation) and `:1093-1096` (render call site, add `suggestions` prop)
- Test: extend `packages/views/squads/components/role-editor.test.tsx` (new file)

**Step 1: Write the failing test**

```tsx
// role-editor.test.tsx
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi } from "vitest";
import { RoleEditor } from "./squad-detail-page";

describe("RoleEditor (combobox)", () => {
  it("commits the typed role on Enter", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<RoleEditor value="" suggestions={[]} onSave={onSave} />);
    await userEvent.click(screen.getByRole("button", { name: /add role/i }));
    const input = screen.getByPlaceholderText(/type or pick/i);
    await userEvent.type(input, "Reviewer{Enter}");
    expect(onSave).toHaveBeenCalledWith("Reviewer");
  });

  it("commits a suggestion on click", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<RoleEditor value="" suggestions={["Reviewer", "Implementer"]} onSave={onSave} />);
    await userEvent.click(screen.getByRole("button", { name: /add role/i }));
    await userEvent.click(screen.getByText("Reviewer"));
    expect(onSave).toHaveBeenCalledWith("Reviewer");
  });

  it("does NOT commit on blur", async () => {
    const onSave = vi.fn();
    render(<RoleEditor value="" suggestions={[]} onSave={onSave} />);
    await userEvent.click(screen.getByRole("button", { name: /add role/i }));
    await userEvent.type(screen.getByPlaceholderText(/type or pick/i), "Partial");
    // Click outside
    await userEvent.click(document.body);
    expect(onSave).not.toHaveBeenCalled();
  });

  it("shows Loader2 while saving", () => {
    render(<RoleEditor value="Reviewer" suggestions={[]} saving onSave={() => Promise.resolve()} />);
    expect(screen.getByTestId("role-editor-saving")).toBeInTheDocument();
  });

  it("renders Pencil icon as a persistent affordance when not saving", () => {
    render(<RoleEditor value="Reviewer" suggestions={[]} onSave={() => Promise.resolve()} />);
    expect(screen.getByTestId("role-editor-pencil")).toBeVisible();
  });
});
```

> Note: `RoleEditor` is currently not exported. **Export it** to allow the test to import it directly (it's an internal helper so export is local — no public API impact).

**Step 2: Run — verify it fails**

```bash
pnpm --filter @multica/views exec vitest run squads/components/role-editor.test.tsx
```

Expected: FAIL — missing `suggestions` prop, no Pencil icon, etc.

**Step 3: Rewrite `RoleEditor`**

Replace lines 645-699:

```tsx
import {
  Command, CommandInput, CommandList, CommandEmpty, CommandGroup, CommandItem,
} from "@multica/ui/components/ui/command";
import { Popover, PopoverContent, PopoverTrigger } from "@multica/ui/components/ui/popover";
import { Pencil, Loader2 } from "lucide-react";

export function RoleEditor({
  value,
  suggestions,
  saving = false,
  onSave,
}: {
  value: string;
  suggestions: string[];
  saving?: boolean;
  onSave: (next: string) => Promise<void>;
}) {
  const { t } = useT("squads");
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");

  // Dedup, drop empty, drop the current value to avoid showing it as a "suggestion".
  const uniqueSuggestions = useMemo(() => {
    const set = new Set<string>();
    for (const s of suggestions) {
      const v = s.trim();
      if (v && v !== value) set.add(v);
    }
    return Array.from(set).sort((a, b) => a.localeCompare(b));
  }, [suggestions, value]);

  const commit = async (next: string) => {
    const trimmed = next.trim();
    if (trimmed === value.trim()) { setOpen(false); return; }
    try {
      await onSave(trimmed);
    } finally {
      setOpen(false);
      setQuery("");
    }
  };

  const trigger = (
    <button
      type="button"
      className="group/role inline-flex items-center gap-1 text-xs text-muted-foreground mt-0.5 text-left hover:text-foreground transition-colors"
      aria-label={value || t(($) => $.role_editor.add_role)}
    >
      <span>{value || t(($) => $.role_editor.add_role)}</span>
      {saving ? (
        <Loader2 data-testid="role-editor-saving" className="size-3 animate-spin" />
      ) : (
        <Pencil data-testid="role-editor-pencil" className="size-3 opacity-60" />
      )}
    </button>
  );

  return (
    <Popover open={open} onOpenChange={(v) => { if (!saving) setOpen(v); }}>
      <PopoverTrigger render={trigger} />
      <PopoverContent className="p-0 w-56" align="start">
        <Command>
          <CommandInput
            value={query}
            onValueChange={setQuery}
            placeholder={t(($) => $.role_editor.search_placeholder)}
            onKeyDown={(e) => {
              if (isImeComposing(e)) return;
              if (e.key === "Enter") { e.preventDefault(); void commit(query); }
              else if (e.key === "Escape") { setOpen(false); setQuery(""); }
            }}
            autoFocus
          />
          <CommandList>
            <CommandEmpty>{t(($) => $.role_editor.no_suggestions)}</CommandEmpty>
            <CommandGroup>
              {uniqueSuggestions.map((s) => (
                <CommandItem key={s} value={s} onSelect={() => void commit(s)}>
                  {s}
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  );
}
```

Key differences from original:
- **Blur no longer commits.** Closing the Popover discards the draft.
- **Enter commits the raw query** (the typed text), enabling free-text entry.
- **Click on a `CommandItem` commits that suggestion.**
- **Pencil icon is always visible** next to the role label — addresses "no edit affordance" gap.
- **During `saving`, the Pencil swaps to `Loader2`** and the Popover is locked (open state can't change).
- **IME guard preserved** for Enter.
- **Empty state**: the trigger shows the translated `+ Add role` string instead of the previous italic placeholder.

**Step 4: Wire suggestions in `MembersTab` (render site at :1093-1096)**

Compute the suggestion list once per render of `MembersTab`:

```tsx
const roleSuggestions = useMemo(
  () => members.map((m) => m.role).filter((r): r is string => !!r),
  [members]
);
```

Then pass to each `RoleEditor`:

```tsx
<RoleEditor
  value={m.role ?? ""}
  suggestions={roleSuggestions}
  onSave={async (next) => { await onUpdateRole(m, next); }}
/>
```

> `saving` is **per-member**. We don't currently have a per-member loading state — `updateRoleMut` is shared. Either:
> (a) Leave `saving` always `false` and rely on optimistic mutation feedback (current behavior).
> (b) Track the in-flight member ID in component state and pass `saving={savingMemberId === m.id}`.
>
> **Recommend (b)** — minimal extra state, addresses the gap-report "no spinner during save" point. Implement in `MembersTab`:
>
> ```tsx
> const [savingMemberId, setSavingMemberId] = useState<string | null>(null);
> // wrap onUpdateRole:
> onSave={async (next) => {
>   setSavingMemberId(m.id);
>   try { await onUpdateRole(m, next); }
>   finally { setSavingMemberId(null); }
> }}
> // pass:
> saving={savingMemberId === m.id}
> ```

**Step 5: Run unit tests**

```bash
pnpm --filter @multica/views exec vitest run squads/components/role-editor.test.tsx
```

Expected: PASS.

**Step 6: Run full views test suite + typecheck**

```bash
pnpm typecheck && pnpm --filter @multica/views test
```

Expected: PASS.

**Step 7: Manual verification (UX checks the gap report called out)**

```bash
make dev
```

- Hover a member row → Pencil is **visible without hover** (persistent affordance)
- Click role → Popover opens with current roles as suggestions
- Type "Reviewer", press Enter → commits, Loader2 flashes on the editing row only
- Type partial text, click elsewhere → Popover closes, **no commit**
- Empty role member → trigger shows `+ Add role`
- IME (Chinese): switch to Pinyin input, type partial pinyin → Enter does NOT commit half-word

**Step 8: Commit**

```bash
git add packages/views/squads/components/squad-detail-page.tsx packages/views/squads/components/role-editor.test.tsx
git commit -m "feat(squad): rewrite RoleEditor as Combobox with suggestions"
```

---

## Task 8: E2E coverage

**Files:**
- Create: `e2e/tests/squad-archive-and-role.spec.ts`

**Step 1: Write the E2E spec**

```ts
import { test, expect } from "@playwright/test";
import { loginAsDefault, createTestApi } from "../helpers";
import type { TestApiClient } from "../fixtures";

let api: TestApiClient;

test.beforeEach(async ({ page }) => {
  api = await createTestApi();
  await loginAsDefault(page);
});

test.afterEach(async () => { await api.cleanup(); });

test("archive dialog shows leader name + active issue count", async ({ page }) => {
  const squad = await api.createSquadWithLeader("Test Squad");
  await api.createIssue({ assignee_type: "squad", assignee_id: squad.id, title: "i1", status: "todo" });
  await api.createIssue({ assignee_type: "squad", assignee_id: squad.id, title: "i2", status: "in_progress" });
  await api.createIssue({ assignee_type: "squad", assignee_id: squad.id, title: "i3", status: "done" });

  await page.goto(`/${api.workspaceSlug}/squads/${squad.id}`);
  await page.getByRole("button", { name: /archive/i }).first().click();
  await expect(page.getByRole("alertdialog")).toContainText(squad.leaderName);
  await expect(page.getByRole("alertdialog")).toContainText("2"); // active only
});

test("role combobox commits on Enter and surfaces existing roles", async ({ page }) => {
  const squad = await api.createSquadWithLeader("S");
  const m = await api.addAgentToSquad(squad.id, { role: "Reviewer" });
  await api.addAgentToSquad(squad.id, { role: "" });

  await page.goto(`/${api.workspaceSlug}/squads/${squad.id}`);
  await page.getByRole("button", { name: /add role/i }).click();
  // Reviewer should appear as a suggestion (aggregated from other members)
  await expect(page.getByText("Reviewer")).toBeVisible();
  await page.getByRole("combobox").fill("Implementer");
  await page.keyboard.press("Enter");
  await expect(page.getByText("Implementer")).toBeVisible();
});
```

**Step 2: Run**

```bash
pnpm exec playwright test e2e/tests/squad-archive-and-role.spec.ts
```

Expected: PASS (with backend + frontend running).

**Step 3: Commit**

```bash
git add e2e/tests/squad-archive-and-role.spec.ts
git commit -m "test(squad): e2e for archive dialog and role combobox"
```

---

## Task 9: Full verification

**Step 1: Full check**

```bash
make check
```

Expected: all green (typecheck, unit, Go, E2E).

**Step 2: Final commit if any cleanup**

No expected cleanup. If anything, run `pnpm lint --fix` and commit.

---

## Risks / Reviewer attention points

1. **Param shape after `make sqlc`** — `CountActiveIssuesForSquadParams.AssigneeID` may be `pgtype.UUID` directly; do not assume a pointer helper. Verify and adjust handler call.
2. **`active_issue_count` is `*int64` + `omitempty` on the wire; `number | null | undefined` (optional) in TS** — see *API Compatibility Notes* and Task 3 for the rationale. Old-server (no field) and count-error (null) collapse to the same "fallback copy" path in the dialog.
3. **`RoleEditor` export change** — internal helper, no consumers outside this file. Verify with `grep -rn "RoleEditor" packages/`.
4. **Per-member saving state** — recommended approach adds local state to `MembersTab`. If the team prefers truly stateless, fall back to "always false" and rely on optimistic mutation behavior.
5. **Archive transfer is now active-only** (resolved decision, see *Archive semantics decision*). Terminal-state issues (`done`, `cancelled`) keep their `assignee_id` pointing at the archived squad row. Squad row stays in the DB (only `archived_at` is set), so existing badge / lookup-by-id code paths continue to resolve the name. No UI changes required for "viewing a closed issue assigned to an archived squad".
6. **No new DB index** (resolved, see *Index assumption*). `idx_issue_assignee (assignee_type, assignee_id)` is sufficient for `CountActiveIssuesForSquad` at realistic squad cardinality (low-hundreds of issues per squad). If profiling later shows the query becomes hot, add the partial index suggested in that section.
7. **API-compat checklist:**
   - [x] `active_issue_count` is additive (old clients ignore it)
   - [x] TS field declared optional — list/create/update responses do not lie about shape
   - [x] Schema is `.loose()` (matches `schemas.ts:25` convention) and tolerates missing / null / numeric values
   - [x] `parseWithFallback` wired in `getSquad`; list/create/update intentionally not wrapped (no consumer)
   - [x] Schema tests cover: missing field, present `null`, present number, unknown future field passthrough
   - [x] Enum drift n/a (no enums in this schema)
   - [x] No reliance on a single boolean for UI affordance — dialog has fallback copy when count is `null` or `undefined`
