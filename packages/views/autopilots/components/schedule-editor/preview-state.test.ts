// @vitest-environment node

import { describe, expect, it } from "vitest";
import { getSchedulePreviewState } from "./preview-state";

type PreviewInput = Parameters<typeof getSchedulePreviewState>[0];
type PreviewState = ReturnType<typeof getSchedulePreviewState>;
type PreviewCase = readonly [string, Partial<PreviewInput>, PreviewState];

const base = {
  advanced: false,
  current: true,
  status: "pending",
  nextRuns: null,
  rejected: false,
  accepted: false,
  hasShownRuns: false,
} satisfies PreviewInput;

const state = (body: PreviewState["body"], overrides: Partial<PreviewState> = {}): PreviewState => ({
  advancedNotice: null,
  body,
  busy: false,
  settled: false,
  unavailable: false,
  ...overrides,
});

const cases = [
  ["pending", {}, state("pending")],
  [
    "accepted",
    {
      advanced: true,
      status: "success",
      nextRuns: ["future"],
      accepted: true,
      hasShownRuns: true,
    },
    state("runs", { advancedNotice: "accepted", settled: true }),
  ],
  [
    "empty",
    { advanced: true, status: "success", nextRuns: [], accepted: true },
    state("empty", { advancedNotice: "accepted", settled: true }),
  ],
  // Rejection details are normalized before this behavior-free state helper.
  ["invalid cron", { status: "error", rejected: true }, state("hidden")],
  ["invalid timezone", { advanced: true, status: "error", rejected: true }, state("hidden")],
  [
    "unavailable",
    { advanced: true, status: "error" },
    state("unavailable", { advancedNotice: "unavailable", unavailable: true }),
  ],
  ["unknown rejection", { advanced: true, status: "error", rejected: true }, state("hidden")],
  ["stale runs", { current: false, hasShownRuns: true }, state("runs", { busy: true })],
  [
    "unreadable success response",
    { advanced: true, status: "success" },
    state("unavailable", { advancedNotice: "unavailable", unavailable: true }),
  ],
  [
    "advanced validation still checking",
    { advanced: true },
    state("pending", { advancedNotice: "checking" }),
  ],
] satisfies PreviewCase[];

describe("getSchedulePreviewState", () => {
  it.each(cases)("maps %s to the canonical preview surfaces", (_name, input, expected) => {
    expect(getSchedulePreviewState({ ...base, ...input })).toEqual(expected);
  });
});
