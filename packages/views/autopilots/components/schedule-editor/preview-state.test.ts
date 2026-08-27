// @vitest-environment node

import { describe, expect, it } from "vitest";
import { getSchedulePreviewState, type PreviewQueryStatus } from "./preview-state";

const base = {
  advanced: false,
  current: true,
  status: "pending" as PreviewQueryStatus,
  nextRuns: null,
  rejected: false,
  accepted: false,
  hasShownRuns: false,
};

describe("getSchedulePreviewState", () => {
  it("maps query verdicts to the canonical preview surfaces", () => {
    const cases = [
      ["pending", {}, { body: "pending", busy: false }],
      ["accepted runs", { status: "success", nextRuns: ["future"], hasShownRuns: true }, { body: "runs", settled: true, busy: false }],
      ["stale runs", { current: false, hasShownRuns: true }, { body: "runs", settled: false, busy: true }],
      ["accepted empty", { status: "success", nextRuns: [] }, { body: "empty", settled: true }],
      ["unreadable response", { status: "success" }, { body: "unavailable", unavailable: true }],
      ["transport failure", { status: "error" }, { body: "unavailable", unavailable: true }],
      ["rejection", { status: "error", rejected: true }, { body: "hidden", unavailable: false }],
      ["accepted advanced", { advanced: true, accepted: true }, { advancedNotice: "accepted" }],
      ["unverified advanced", { advanced: true, status: "error" }, { advancedNotice: "unavailable" }],
      ["checking advanced", { advanced: true }, { advancedNotice: "checking" }],
    ] as const;

    for (const [name, input, expected] of cases) {
      expect(getSchedulePreviewState({ ...base, ...input }), name).toMatchObject(expected);
    }
  });
});
