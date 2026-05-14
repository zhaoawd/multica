import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type {
  GitHubPullRequest,
  GitHubPullRequestChecksConclusion,
} from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";

const TEST_RESOURCES = { en: { common: enCommon, issues: enIssues } };

vi.mock("@multica/core/github/queries", async () => {
  const actual = await vi.importActual<typeof import("@multica/core/github/queries")>(
    "@multica/core/github/queries",
  );
  return {
    ...actual,
    issuePullRequestsOptions: (issueId: string) => ({
      queryKey: ["github", "pull-requests", issueId],
      queryFn: async () => ({ pull_requests: mockPRs }),
      enabled: !!issueId,
    }),
  };
});

import { PullRequestList } from "./pull-request-list";

let mockPRs: GitHubPullRequest[] = [];

function makePR(overrides: Partial<GitHubPullRequest> = {}): GitHubPullRequest {
  return {
    id: "pr-1",
    workspace_id: "ws-1",
    repo_owner: "acme",
    repo_name: "widget",
    number: 1,
    title: "Test PR",
    state: "open",
    html_url: "https://example.test/pr/1",
    branch: "feat/x",
    author_login: "octocat",
    author_avatar_url: null,
    merged_at: null,
    closed_at: null,
    pr_created_at: "2026-01-01T00:00:00Z",
    pr_updated_at: "2026-01-01T00:00:00Z",
    mergeable_state: null,
    checks_conclusion: null,
    ...overrides,
  };
}

function renderList() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <I18nProvider resources={TEST_RESOURCES} locale="en">
        <PullRequestList issueId="issue-1" />
      </I18nProvider>
    </QueryClientProvider>,
  );
}

async function waitForBadges() {
  // The list is rendered as soon as the (mocked) query resolves.
  return screen.findAllByRole("link");
}

describe("PullRequestList badges", () => {
  it.each([
    ["passed", /Checks passed/i],
    ["failed", /Checks failed/i],
    ["pending", /Checks pending/i],
  ] as Array<[GitHubPullRequestChecksConclusion, RegExp]>)(
    "renders %s checks badge",
    async (conclusion, label) => {
      mockPRs = [makePR({ checks_conclusion: conclusion })];
      renderList();
      await waitForBadges();
      expect(screen.getByText(label)).toBeInTheDocument();
    },
  );

  it("renders dirty conflicts badge for mergeable_state=dirty", async () => {
    mockPRs = [makePR({ mergeable_state: "dirty" })];
    renderList();
    await waitForBadges();
    expect(screen.getByText(/Conflicts/i)).toBeInTheDocument();
  });

  it("renders clean conflicts badge for mergeable_state=clean", async () => {
    mockPRs = [makePR({ mergeable_state: "clean" })];
    renderList();
    await waitForBadges();
    expect(screen.getByText(/No conflicts/i)).toBeInTheDocument();
  });

  it("hides conflicts badge for opaque mergeable_state values", async () => {
    mockPRs = [makePR({ mergeable_state: "blocked" })];
    renderList();
    await waitForBadges();
    expect(screen.queryByText(/Conflicts|No conflicts/i)).not.toBeInTheDocument();
  });

  it("hides status row for terminal PR states", async () => {
    mockPRs = [makePR({ state: "merged", checks_conclusion: "passed", mergeable_state: "clean" })];
    renderList();
    await waitForBadges();
    expect(screen.queryByText(/Checks passed/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/No conflicts/i)).not.toBeInTheDocument();
  });

  it("hides everything when both fields are null (legacy backend)", async () => {
    mockPRs = [makePR()];
    renderList();
    await waitForBadges();
    expect(screen.queryByText(/Checks/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/Conflicts|No conflicts/i)).not.toBeInTheDocument();
  });
});
