export type PreviewQueryStatus = "pending" | "success" | "error";
export type AdvancedNotice = "accepted" | "unavailable" | "checking" | null;
export type PreviewBody = "hidden" | "unavailable" | "runs" | "empty" | "pending";

export interface SchedulePreviewState {
  advancedNotice: AdvancedNotice;
  body: PreviewBody;
  busy: boolean;
  settled: boolean;
  unavailable: boolean;
}

/** Decide which preview surfaces are trustworthy for the current schedule. */
export function getSchedulePreviewState({
  advanced,
  current,
  status,
  nextRuns,
  rejected,
  accepted,
  hasShownRuns,
}: {
  advanced: boolean;
  current: boolean;
  status: PreviewQueryStatus;
  nextRuns: readonly string[] | null;
  rejected: boolean;
  accepted: boolean;
  hasShownRuns: boolean;
}): SchedulePreviewState {
  const settled = current && status === "success" && nextRuns !== null;
  const unavailable =
    current &&
    ((status === "error" && !rejected) || (status === "success" && nextRuns === null));
  const showsRuns = !rejected && !unavailable && hasShownRuns;
  const body: PreviewBody = rejected
    ? "hidden"
    : unavailable
      ? "unavailable"
      : showsRuns
        ? "runs"
        : settled
          ? "empty"
          : "pending";
  const advancedNotice: AdvancedNotice =
    !advanced || rejected
      ? null
      : accepted
        ? "accepted"
        : unavailable
          ? "unavailable"
          : "checking";

  return {
    advancedNotice,
    body,
    busy: showsRuns && !settled,
    settled,
    unavailable,
  };
}
