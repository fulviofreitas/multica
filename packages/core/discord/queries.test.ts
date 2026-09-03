// @vitest-environment node

import { describe, expect, it } from "vitest";
import { discordInstallationsOptions, discordKeys } from "./queries";

describe("discordKeys", () => {
  it("scopes every key under the workspace id", () => {
    expect(discordKeys.all("ws-1")).toEqual(["discord", "ws-1"]);
    expect(discordKeys.installations("ws-1")).toEqual(["discord", "ws-1", "installations"]);
    // Different workspaces must never collide on the same cache entry.
    expect(discordKeys.installations("ws-1")).not.toEqual(discordKeys.installations("ws-2"));
  });
});

describe("discordInstallationsOptions", () => {
  it("is workspace-scoped and enabled once a workspace id is present", () => {
    const options = discordInstallationsOptions("ws-1");
    expect(options.queryKey).toEqual(discordKeys.installations("ws-1"));
    expect(options.enabled).toBe(true);
  });

  it("stays disabled before workspace context is available", () => {
    expect(discordInstallationsOptions("").enabled).toBe(false);
  });
});
