// @vitest-environment jsdom

import type { ReactNode } from "react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, cleanup } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

// We control the API surface from the test so we can assert exact requests.
const mockGetLarkBinding = vi.hoisted(() => vi.fn());
const mockUpsertLarkBinding = vi.hoisted(() => vi.fn());
const mockDeleteLarkBinding = vi.hoisted(() => vi.fn());
const mockListGitHubInstallations = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({
    queryKey: ["members", "ws-1"],
    queryFn: async () => [{ user_id: "u-1", role: "owner" }],
  }),
}));

vi.mock("@multica/core/github/queries", () => ({
  githubInstallationsOptions: () => ({
    queryKey: ["github", "ws-1", "installations"],
    queryFn: () => mockListGitHubInstallations(),
  }),
}));

vi.mock("@multica/core/api", () => ({
  api: {
    getLarkBinding: mockGetLarkBinding,
    upsertLarkBinding: mockUpsertLarkBinding,
    deleteLarkBinding: mockDeleteLarkBinding,
    getGitHubConnectURL: vi.fn(),
    listGitHubInstallations: mockListGitHubInstallations,
  },
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (sel?: (s: { user: { id: string } }) => unknown) =>
      sel ? sel({ user: { id: "u-1" } }) : { user: { id: "u-1" } },
    { getState: () => ({ user: { id: "u-1" } }) },
  );
  return { useAuthStore };
});

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

import { IntegrationsTab } from "./integrations-tab";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings } };

function Wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        {children}
      </I18nProvider>
    </QueryClientProvider>
  );
}

describe("IntegrationsTab — Lark card", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    cleanup();
    mockListGitHubInstallations.mockResolvedValue({ installations: [], configured: false });
  });

  it("hides the binding form when server reports configured=false", async () => {
    mockGetLarkBinding.mockResolvedValue({
      bound: false,
      configured: false,
      enabled_events: [],
      supported_events: ["issue:created", "issue:updated"],
    });

    render(<IntegrationsTab />, { wrapper: Wrapper });

    // The 'not_configured' env hint should be the surface a user sees.
    await screen.findByText((t) =>
      t.includes("Lark integration is not configured for this deployment"),
    );
    expect(screen.queryByLabelText("Chat ID")).toBeNull();
  });

  it("shows the form with server-driven event list when configured", async () => {
    mockGetLarkBinding.mockResolvedValue({
      bound: false,
      configured: true,
      enabled_events: [],
      supported_events: ["issue:created", "task:failed"],
    });

    render(<IntegrationsTab />, { wrapper: Wrapper });

    await screen.findByLabelText("Chat ID");
    // Only the two supported events provided by the server should render.
    expect(screen.getByText("Issue created")).toBeTruthy();
    expect(screen.getByText("Task failed")).toBeTruthy();
    expect(screen.queryByText("Task completed")).toBeNull();
  });

  it("submits chat_id and selected events on save, then disconnects", async () => {
    mockGetLarkBinding.mockResolvedValue({
      bound: false,
      configured: true,
      enabled_events: [],
      supported_events: ["issue:created", "task:completed"],
    });
    mockUpsertLarkBinding.mockImplementation(async (_ws, body) => ({
      bound: true,
      configured: true,
      chat_id: body.chat_id,
      enabled_events: body.enabled_events,
      supported_events: ["issue:created", "task:completed"],
    }));
    mockDeleteLarkBinding.mockResolvedValue(undefined);

    const user = userEvent.setup();
    render(<IntegrationsTab />, { wrapper: Wrapper });

    const input = await screen.findByLabelText("Chat ID");
    await user.type(input, "oc_test_chat");

    // Toggle the first event on (issue:created).
    const issueCreatedRow = screen.getByText("Issue created").closest("label")!;
    await user.click(issueCreatedRow);

    await user.click(screen.getByRole("button", { name: "Connect Lark" }));

    await waitFor(() =>
      expect(mockUpsertLarkBinding).toHaveBeenCalledWith("ws-1", {
        chat_id: "oc_test_chat",
        enabled_events: ["issue:created"],
      }),
    );

    // Disconnect button surfaces only after the binding is established.
    const disconnect = await screen.findByRole("button", { name: "Disconnect" });
    await user.click(disconnect);
    await waitFor(() => expect(mockDeleteLarkBinding).toHaveBeenCalledWith("ws-1"));
  });
});
