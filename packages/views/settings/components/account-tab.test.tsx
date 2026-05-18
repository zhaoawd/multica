// @vitest-environment jsdom

import type { ReactNode } from "react";
import { describe, it, expect, beforeEach, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const mockGetMyLarkUserLink = vi.hoisted(() => vi.fn());
const mockStartLarkUserLink = vi.hoisted(() => vi.fn());
const mockDeleteMyLarkUserLink = vi.hoisted(() => vi.fn());
const mockToastSuccess = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    getMyLarkUserLink: mockGetMyLarkUserLink,
    startLarkUserLink: mockStartLarkUserLink,
    deleteMyLarkUserLink: mockDeleteMyLarkUserLink,
    // updateMe / upload aren't exercised but the AccountTab imports `api`.
    updateMe: vi.fn(),
  },
}));

vi.mock("@multica/core/auth", () => {
  const setUser = vi.fn();
  const useAuthStore = Object.assign(
    (sel?: (s: { user: { id: string; name: string; avatar_url?: string }; setUser: typeof setUser }) => unknown) =>
      sel
        ? sel({ user: { id: "u-1", name: "Test User" }, setUser })
        : { user: { id: "u-1", name: "Test User" }, setUser },
    { getState: () => ({ user: { id: "u-1", name: "Test User" }, setUser }) },
  );
  return { useAuthStore };
});

vi.mock("@multica/core/hooks/use-file-upload", () => ({
  useFileUpload: () => ({ upload: vi.fn(), uploading: false }),
}));

vi.mock("sonner", () => ({
  toast: { success: mockToastSuccess, error: mockToastError },
}));

import { AccountTab } from "./account-tab";

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

describe("AccountTab — Linked Accounts (Lark)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    cleanup();
    // Reset URL between tests — the component reads window.location.search.
    window.history.replaceState({}, "", "/settings");
  });

  it("shows a disabled Connect button when the server reports not configured", async () => {
    mockGetMyLarkUserLink.mockResolvedValue({ linked: false, configured: false });

    render(<AccountTab />, { wrapper: Wrapper });

    await screen.findByText(/Lark integration is not configured on this server/i);
    const connect = await screen.findByRole("button", { name: /Connect Lark/i });
    expect((connect as HTMLButtonElement).disabled).toBe(true);
  });

  it("starts OAuth and navigates the browser to the returned authorize URL", async () => {
    mockGetMyLarkUserLink.mockResolvedValue({ linked: false, configured: true });
    mockStartLarkUserLink.mockResolvedValue({ url: "https://accounts.feishu.cn/authorize?x=1" });

    window.history.replaceState({}, "", "/my-workspace/settings");
    // jsdom's window.location.assign is non-configurable, so swap the whole
    // location object via Object.defineProperty (configurable: true so we
    // can put it back after this test).
    const assign = vi.fn();
    const originalLocation = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, pathname: "/my-workspace/settings", search: "", assign },
    });

    try {
      render(<AccountTab />, { wrapper: Wrapper });

      const connect = await screen.findByRole("button", { name: /Connect Lark/i });
      await userEvent.click(connect);

      await waitFor(() => {
        expect(mockStartLarkUserLink).toHaveBeenCalledWith("/my-workspace/settings");
      });
      expect(assign).toHaveBeenCalledWith("https://accounts.feishu.cn/authorize?x=1");
    } finally {
      Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
    }
  });

  it("shows the linked state and disconnects on click", async () => {
    mockGetMyLarkUserLink.mockResolvedValue({
      linked: true,
      configured: true,
      open_id: "ou_test_123",
    });
    mockDeleteMyLarkUserLink.mockResolvedValue(undefined);

    render(<AccountTab />, { wrapper: Wrapper });

    await screen.findByText("ou_test_123");
    const disconnect = await screen.findByRole("button", { name: /^Disconnect$/i });
    await userEvent.click(disconnect);

    await waitFor(() => expect(mockDeleteMyLarkUserLink).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(mockToastSuccess).toHaveBeenCalled());
  });

  it("pops a success toast when the URL carries ?lark_linked=1 and strips the param", async () => {
    mockGetMyLarkUserLink.mockResolvedValue({ linked: true, configured: true, open_id: "ou_a" });
    window.history.replaceState({}, "", "/my-workspace/settings?lark_linked=1");

    render(<AccountTab />, { wrapper: Wrapper });

    await waitFor(() => expect(mockToastSuccess).toHaveBeenCalled());
    expect(window.location.search).not.toContain("lark_linked");
  });

  it("pops an error toast for ?lark_error=invalid_state", async () => {
    mockGetMyLarkUserLink.mockResolvedValue({ linked: false, configured: true });
    window.history.replaceState({}, "", "/my-workspace/settings?lark_error=invalid_state");

    render(<AccountTab />, { wrapper: Wrapper });

    await waitFor(() =>
      expect(mockToastError).toHaveBeenCalledWith(expect.stringMatching(/Lark sign-in expired/i)),
    );
    expect(window.location.search).not.toContain("lark_error");
  });
});
