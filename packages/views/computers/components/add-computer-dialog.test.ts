import { describe, expect, it } from "vitest";
import { buildInstallCommand } from "./add-computer-dialog";

describe("buildInstallCommand", () => {
  it("uses bash because scripts/install.sh is bash-only", () => {
    const command = buildInstallCommand({
      workspaceSlug: "acme",
      token: "mit_abc123",
      apiBaseUrl: "https://api.example.com",
    });

    expect(command).toContain("| bash -s --");
    expect(command).not.toContain("| sh -s --");
  });

  it("resolves an empty API base URL to the browser origin", () => {
    const command = buildInstallCommand({
      workspaceSlug: "acme",
      token: "mit_abc123",
      apiBaseUrl: "",
      browserOrigin: "https://app.example.com",
    });

    expect(command).toContain("--server-url https://app.example.com");
    expect(command).not.toContain("--server-url ''");
  });

  it("turns a relative API base URL into an absolute server URL", () => {
    const command = buildInstallCommand({
      workspaceSlug: "acme",
      token: "mit_abc123",
      apiBaseUrl: "/backend",
      browserOrigin: "https://app.example.com",
    });

    expect(command).toContain("--server-url https://app.example.com/backend");
  });
});
