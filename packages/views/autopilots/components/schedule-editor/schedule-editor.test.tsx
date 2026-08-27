import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import { renderWithI18n } from "../../../test/i18n";
import { ScheduleEditor } from "./schedule-editor";
import type { ScheduleConfig } from "./model";
import { cronFields, parseCron, toCron } from "./cron-mapping";

vi.setConfig({ testTimeout: 15_000 });

const previewMode: { value: "ok" | "transport" | "timezone" | "expired" } = {
  value: "ok",
};
const previewCalls: string[] = [];

vi.mock("@multica/core/autopilots/queries", () => ({
  cronPreviewOptions: (
    wsId: string,
    expr: string,
    timezone: string,
    options?: { enabled?: boolean },
  ) => ({
    queryKey: ["autopilots", wsId, "cron-preview", expr, timezone, previewMode.value],
    queryFn: async () => {
      previewCalls.push(expr);
      if (previewMode.value === "expired") return { next_runs: ["2020-01-01T00:00:00Z"] };
      if (previewMode.value === "transport") {
        throw new ApiError("API error: 500 Internal Server Error", 500, "Internal Server Error");
      }
      if (previewMode.value === "timezone") {
        throw new ApiError(`invalid timezone "${timezone}"`, 400, "Bad Request", {
          error: `invalid timezone "${timezone}"`,
          code: "invalid_timezone",
        });
      }

      let fields = expr;
      if (fields.startsWith("TZ=") || fields.startsWith("CRON_TZ=")) {
        const space = fields.indexOf(" ");
        const zone = space < 0 ? null : fields.slice(fields.indexOf("=") + 1, space);
        if (zone === null || !["UTC", "Asia/Tokyo", "Asia/Shanghai", "Local"].includes(zone)) {
          throw new ApiError("parse cron: provided bad location", 400, "Bad Request", {
            error: "parse cron: provided bad location",
            code: "invalid_cron",
          });
        }
        fields = fields.slice(space).trim();
      }
      if (fields.trim().split(/\s+/).length !== 5) {
        throw new ApiError("parse cron: expected exactly 5 fields", 400, "Bad Request", {
          error: "parse cron: expected exactly 5 fields",
          code: "invalid_cron",
        });
      }
      if (fields === "0 0 30 2 *") return { next_runs: [] };
      return {
        next_runs: ["2126-07-14T01:00:00Z", "2126-07-14T03:00:00Z"],
      };
    },
    enabled: options?.enabled ?? true,
    retry: false,
  }),
}));

vi.mock("../pickers/timezone-picker", () => ({
  TimezonePicker: ({ value, onChange }: { value: string; onChange: (value: string) => void }) => (
    <select data-testid="timezone-picker" value={value} onChange={(event) => onChange(event.target.value)}>
      <option value="UTC">UTC</option>
      <option value="Asia/Shanghai">Asia/Shanghai</option>
    </select>
  ),
}));

function Harness({
  initial,
  onChange,
  disabled,
}: {
  initial: ScheduleConfig;
  onChange?: () => void;
  disabled?: boolean;
}) {
  const [value, setValue] = useState(initial);
  const [valid, setValid] = useState(true);
  return (
    <>
      <ScheduleEditor
        value={value}
        onChange={(next) => {
          onChange?.();
          setValue(next);
        }}
        wsId="ws-test"
        disabled={disabled}
        onValidityChange={setValid}
      />
      <output data-testid="cron-out">{cronFields(value)}</output>
      <output data-testid="wire-out">{toCron(value)}</output>
      <output data-testid="raw-out">{String(value.raw)}</output>
      <output data-testid="tz-out">{value.timezone}</output>
      <output data-testid="valid-out">{String(valid)}</output>
    </>
  );
}

function renderEditor(initial: ScheduleConfig, options?: { onChange?: () => void; disabled?: boolean }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={queryClient}>
      <Harness initial={initial} onChange={options?.onChange} disabled={options?.disabled} />
    </QueryClientProvider>,
  );
}

const cron = (expression: string) => parseCron(expression, "UTC");
const cronOut = () => screen.getByTestId("cron-out").textContent;
const wireOut = () => screen.getByTestId("wire-out").textContent;

async function openCronInput(): Promise<HTMLElement> {
  if (screen.queryByRole("textbox", { name: "Cron" }) === null) {
    await userEvent.setup().click(screen.getByRole("button", { name: /click to edit/ }));
  }
  return screen.getByRole("textbox", { name: "Cron" });
}

async function editCronText(expression: string) {
  const input = await openCronInput();
  fireEvent.change(input, { target: { value: expression } });
  fireEvent.blur(input);
}

async function choose(label: string, option: string) {
  const user = userEvent.setup();
  await user.click(screen.getByLabelText(label));
  await user.click(await screen.findByRole("option", { name: option }));
}

describe("ScheduleEditor", () => {
  // Pure parsing, transitions, preview decisions, and number-field boundaries
  // are canonical in sibling .test.ts/.test.tsx suites. This suite owns DOM
  // wiring, accessibility, and integration-specific regressions only.
  it("renders the editor without changing an untouched schedule", async () => {
    const onChange = vi.fn();
    renderEditor(cron("0 9-21 * * *"), { onChange });
    expect(screen.getByText("Time")).toBeInTheDocument();
    expect(screen.getByText("Days")).toBeInTheDocument();
    expect(screen.getByText("Timezone")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /click to edit/ })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("Next runs")).toBeInTheDocument());
    expect(onChange).not.toHaveBeenCalled();
  });

  it("wires interval, window, and day edits into one schedule", async () => {
    const user = userEvent.setup();
    renderEditor(cron("0 9-21/2 * * 2-4"));
    fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "3" } });
    await user.click(screen.getByLabelText("Window end hour"));
    await user.keyboard("18");
    await user.click(screen.getByRole("button", { name: "Monday" }));
    await user.click(screen.getByLabelText("Window start hour"));
    await user.keyboard("10");
    expect(cronOut()).toBe("0 10-18/3 * * 1-4");
  });

  it("hydrates the structured controls from committed cron text", async () => {
    renderEditor(cron("0 9 * * *"));
    await editCronText("0 9-21/2 * * 2-4");
    expect(cronOut()).toBe("0 9-21/2 * * 2-4");
    expect(screen.getByTestId("raw-out")).toHaveTextContent("null");
    expect(screen.getByRole("button", { name: "Tuesday", pressed: true })).toBeInTheDocument();
  });

  it("preserves structured values under advanced text and recovers them", async () => {
    renderEditor(cron("0 17 * * 1"));
    await editCronText("0 9 1,15 * *");
    await waitFor(() => expect(screen.getByText(/visual editor can't represent/)).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "At a time" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Monday", pressed: true })).toBeInTheDocument();
    expect(screen.getByLabelText("Hour")).toHaveValue("17");

    await editCronText("0 9 * * *");
    expect(screen.getByTestId("raw-out")).toHaveTextContent("null");
    expect(screen.getByRole("button", { name: "At a time" })).toBeEnabled();
  });

  it("re-queries a timezone edit without rewriting cron fields", async () => {
    renderEditor(cron("0 9-21/2 * * 2-4"));
    const preview = await waitFor(() => {
      const line = screen.getByText("Next runs").closest("div");
      expect(line).toHaveAttribute("aria-busy", "false");
      return line;
    });
    fireEvent.change(screen.getByTestId("timezone-picker"), {
      target: { value: "Asia/Shanghai" },
    });
    expect(cronOut()).toBe("0 9-21/2 * * 2-4");
    expect(screen.getByTestId("raw-out")).toHaveTextContent("null");
    expect(preview).toHaveAttribute("aria-busy", "true");
    await waitFor(() => expect(preview).toHaveAttribute("aria-busy", "false"));
  });

  it("shows a server rejection inline and blocks saving", async () => {
    renderEditor(cron("0 9 * * *"));
    await editCronText("@daily");
    await waitFor(() => expect(screen.getByText(/expected exactly 5 fields/)).toBeInTheDocument());
    expect(screen.getByTestId("valid-out")).toHaveTextContent("false");
    expect(screen.queryByText("Next runs")).not.toBeInTheDocument();
    expect(screen.queryByText(/visual editor can't represent/)).not.toBeInTheDocument();
  });

  it("commits cron only on Enter or blur and ignores an empty commit", async () => {
    renderEditor(cron("0 9 * * *"));
    const input = await openCronInput();
    fireEvent.change(input, { target: { value: "0 18 * * *" } });
    expect(cronOut()).toBe("0 9 * * *");
    fireEvent.keyDown(input, { key: "Enter" });
    expect(cronOut()).toBe("0 18 * * *");
    expect(input).toHaveFocus();
    fireEvent.change(input, { target: { value: "   " } });
    fireEvent.blur(input);
    expect(cronOut()).toBe("0 18 * * *");
    expect(screen.getByTestId("raw-out")).toHaveTextContent("null");
  });

  it("locks every editor control while retaining the read-only preview", async () => {
    renderEditor(cron("0 9 * * 1-5"), { disabled: true });
    expect(screen.getByRole("button", { name: "At a time" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Monday" })).toBeDisabled();
    expect(screen.getByRole("button", { name: /click to edit/ })).toBeDisabled();
    await waitFor(() => expect(screen.getByText("Next runs")).toBeInTheDocument());
  });

  it("names controls and focuses fields revealed by mode switches", async () => {
    const user = userEvent.setup();
    renderEditor(cron("0 9 * * *"));
    expect(screen.getByLabelText("Hour")).toBeInTheDocument();
    expect(screen.getByLabelText("Minute")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "At an interval" }));
    expect(screen.getByLabelText("Interval")).toHaveFocus();
    expect(screen.getByLabelText("Window start hour")).toBeInTheDocument();
    await choose("Interval unit", "minutes");
    expect(screen.getByLabelText("Interval")).toHaveFocus();
    await choose("Day pattern", "Day of month");
    expect(screen.getByLabelText("Day of month")).toHaveFocus();
  });

  it("does not steal focus from a field already shown on first render", () => {
    renderEditor(cron("0 9 14 * *"));
    expect(screen.getByLabelText("Day of month")).not.toHaveFocus();
  });

  it("groups compound controls and renders human-readable select labels", () => {
    renderEditor(cron("*/15 9-21 * * *"));
    const step = screen.getByLabelText("Interval");
    const unit = screen.getByLabelText("Interval unit");
    expect(step.closest("[data-slot=input-group]")).toContainElement(unit);
    const start = screen.getByLabelText("Window start hour");
    expect(start.closest("[data-slot=input-group]")).toContainElement(
      screen.getByLabelText("Window end hour"),
    );
    const labels = screen.getAllByRole("combobox").map((element) => element.textContent ?? "");
    expect(labels.some((label) => label.startsWith("Every day"))).toBe(true);
    expect(labels.some((label) => label.startsWith("minutes"))).toBe(true);
  });

  it("edits an hour-only window for a minute interval", async () => {
    const user = userEvent.setup();
    renderEditor(cron("*/15 9-21 * * *"));
    expect(screen.queryByLabelText("Window start minute")).not.toBeInTheDocument();
    await user.click(screen.getByLabelText("Window start hour"));
    await user.keyboard("10");
    await user.click(screen.getByLabelText("Window end hour"));
    await user.keyboard("18");
    expect(cronOut()).toBe("*/15 10-18 * * *");
  });

  it("narrows an always-visible all-day window in one edit", async () => {
    const user = userEvent.setup();
    renderEditor(cron("15 */3 * * *"));
    expect(screen.getByLabelText("Window start hour")).toHaveValue("00");
    expect(screen.getByLabelText("Window start minute")).toHaveValue("15");
    expect(screen.getByLabelText("Window end hour")).toHaveValue("23");
    await user.click(screen.getByLabelText("Window start hour"));
    await user.keyboard("09");
    expect(cronOut()).toBe("15 9-23/3 * * *");
  });

  it("restores interval settings after a round trip through fixed time", async () => {
    const user = userEvent.setup();
    renderEditor(cron("30 9-21/3 * * 1-5"));
    await user.click(screen.getByRole("button", { name: "At a time" }));
    await user.click(screen.getByRole("button", { name: "At an interval" }));
    expect(cronOut()).toBe("30 9-21/3 * * 1-5");
  });

  it("restores weekly and monthly values after switching day kinds", async () => {
    renderEditor(cron("0 9 * * 2,4,6"));
    await choose("Day pattern", "Day of month");
    fireEvent.change(screen.getByLabelText("Day of month"), { target: { value: "30" } });
    await choose("Day pattern", "Every day");
    await choose("Day pattern", "Days of week");
    expect(cronOut()).toBe("0 9 * * 2,4,6");
    await choose("Day pattern", "Day of month");
    expect(screen.getByLabelText("Day of month")).toHaveValue(30);
    expect(screen.getByText("Months without day 30 are skipped.")).toBeInTheDocument();
  });

  it("restores a dragged window end but keeps an end explicitly typed after a drag", async () => {
    const user = userEvent.setup();
    renderEditor(cron("0 9-15/2 * * *"));
    const start = screen.getByLabelText("Window start hour");
    await user.click(start);
    await user.keyboard("22");
    await user.click(start);
    await user.keyboard("09");
    expect(cronOut()).toBe("0 9-15/2 * * *");

    await user.click(start);
    await user.keyboard("22");
    await user.click(screen.getByLabelText("Window end hour"));
    await user.keyboard("22");
    await user.click(start);
    await user.keyboard("09");
    expect(cronOut()).toBe("0 9-22/2 * * *");
  });

  it("keeps stale next runs mounted and marks them busy during an edit", async () => {
    renderEditor(cron("0 9 * * *"));
    const line = await waitFor(() => screen.getByText("Next runs").closest("div"));
    expect(line).not.toBeNull();
    await editCronText("0 18 * * *");
    expect(screen.getByText("Next runs").closest("div")).toBe(line);
    expect(line).toHaveAttribute("aria-busy", "true");
    await waitFor(() => expect(line).toHaveAttribute("aria-busy", "false"));
  });

  it("refreshes an expired preview once for each expression", async () => {
    previewMode.value = "expired";
    previewCalls.length = 0;
    try {
      renderEditor(cron("0 9 * * *"));
      await waitFor(() => expect(previewCalls.filter((value) => value.includes("0 9 * * *"))).toHaveLength(2));
      await editCronText("0 9 * * 1-5");
      await waitFor(() => expect(previewCalls.filter((value) => value.includes("0 9 * * 1-5"))).toHaveLength(2));
    } finally {
      previewMode.value = "ok";
      previewCalls.length = 0;
    }
  });

  it("treats an unavailable advanced preview as unverified but still saveable", async () => {
    previewMode.value = "transport";
    try {
      renderEditor(cron("0 9 1,15 * *"));
      await waitFor(() => expect(screen.getByText(/server couldn't be reached/)).toBeInTheDocument());
      expect(screen.getByText(/Next runs unavailable/)).toBeInTheDocument();
      expect(screen.queryByText(/visual editor can't represent/)).not.toBeInTheDocument();
      expect(screen.getByTestId("valid-out")).toHaveTextContent("true");
    } finally {
      previewMode.value = "ok";
    }
  });

  it("accepts an advanced expression with no upcoming runs", async () => {
    renderEditor(cron("0 0 30 2 *"));
    await waitFor(() => expect(screen.getByText(/visual editor can't represent/)).toBeInTheDocument());
    expect(screen.getByText(/no upcoming runs/)).toBeInTheDocument();
  });

  it("attributes a timezone rejection to the picker rather than the cron", async () => {
    previewMode.value = "timezone";
    try {
      renderEditor(cron("0 9 * * *"));
      await waitFor(() => expect(screen.getByText(/timezone isn't recognized/)).toBeInTheDocument());
      expect(screen.queryByText("This cron expression isn't valid.")).not.toBeInTheDocument();
    } finally {
      previewMode.value = "ok";
    }
  });

  it("promotes an accepted typed timezone prefix into the picker", async () => {
    renderEditor(cron("0 9 * * *"));
    await editCronText("CRON_TZ=Asia/Tokyo 30 9 * * 1-5");
    expect(screen.getByTestId("raw-out")).toHaveTextContent("CRON_TZ=Asia/Tokyo");
    await waitFor(() => expect(screen.getByTestId("tz-out")).toHaveTextContent("Asia/Tokyo"));
    expect(screen.getByTestId("raw-out")).toHaveTextContent("null");
    expect(cronOut()).toBe("30 9 * * 1-5");
  });

  it("keeps a rejected typed timezone prefix verbatim", async () => {
    renderEditor(cron("0 9 * * *"));
    await editCronText("TZ=Europe/Berlin 0 9 * * *");
    await waitFor(() => expect(screen.getByText("This cron expression isn't valid.")).toBeInTheDocument());
    expect(screen.getByTestId("raw-out")).toHaveTextContent("TZ=Europe/Berlin 0 9 * * *");
    expect(screen.getByTestId("tz-out")).toHaveTextContent("UTC");
  });

  it("uses a fixed timezone segment in the wire form without stealing focus", async () => {
    renderEditor(cron("0 9 * * *"));
    expect(wireOut()).toBe("TZ=UTC 0 9 * * *");
    const input = await openCronInput();
    input.focus();
    expect(input).toHaveValue("0 9 * * *");
    expect(fireEvent.mouseDown(screen.getByText("TZ=UTC"))).toBe(false);
    expect(input).toHaveFocus();

    previewCalls.length = 0;
    fireEvent.change(screen.getByTestId("timezone-picker"), {
      target: { value: "Asia/Shanghai" },
    });
    expect(wireOut()).toBe("TZ=Asia/Shanghai 0 9 * * *");
    await waitFor(() => expect(previewCalls).toContain("TZ=Asia/Shanghai 0 9 * * *"));
  });

  it("does not add a second prefix while a typed prefix awaits promotion", async () => {
    renderEditor(cron("0 9 * * *"));
    await editCronText("TZ=Local 0 9 * * *");
    await waitFor(() => expect(screen.getByText("Next runs")).toBeInTheDocument());
    expect(screen.getByTestId("raw-out")).toHaveTextContent("TZ=Local 0 9 * * *");
    expect(wireOut()).toBe("TZ=Local 0 9 * * *");
    const input = await openCronInput();
    expect(input).toHaveValue("TZ=Local 0 9 * * *");
    expect(screen.queryByText("TZ=UTC")).not.toBeInTheDocument();
  });
});
