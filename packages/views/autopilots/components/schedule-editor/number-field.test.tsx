import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { NumberField } from "./number-field";

describe("NumberField", () => {
  it("keeps both digits while committing complete in-range values", () => {
    const onCommit = vi.fn();
    render(<NumberField value={15} min={1} max={31} ariaLabel="Day" onCommit={onCommit} />);
    const input = screen.getByLabelText("Day");
    fireEvent.change(input, { target: { value: "1" } });
    fireEvent.change(input, { target: { value: "18" } });
    expect(input).toHaveValue(18);
    expect(onCommit.mock.calls).toEqual([[1], [18]]);
  });

  it("restores empty text and clamps out-of-range text on blur", () => {
    const restore = vi.fn();
    const clamp = vi.fn();
    render(
      <>
        <NumberField value={15} min={1} max={31} ariaLabel="Restore" onCommit={restore} />
        <NumberField value={6} min={1} max={23} ariaLabel="Clamp" onCommit={clamp} />
      </>,
    );
    const restoreInput = screen.getByLabelText("Restore");
    fireEvent.change(restoreInput, { target: { value: "" } });
    fireEvent.blur(restoreInput);
    expect(restoreInput).toHaveValue(15);
    expect(restore).not.toHaveBeenCalled();

    const clampInput = screen.getByLabelText("Clamp");
    fireEvent.change(clampInput, { target: { value: "24" } });
    fireEvent.blur(clampInput);
    expect(clampInput).toHaveValue(23);
    expect(clamp).toHaveBeenLastCalledWith(23);
  });

  it.each([
    ["0", "ArrowUp", 1],
    ["40", "ArrowDown", 31],
    ["31", "ArrowUp", 1],
    ["1", "ArrowDown", 31],
  ])("steps %s with %s to %i", (typed, key, expected) => {
    const onCommit = vi.fn();
    render(<NumberField value={15} min={1} max={31} ariaLabel="Day" onCommit={onCommit} />);
    const input = screen.getByLabelText("Day");
    fireEvent.change(input, { target: { value: typed } });
    fireEvent.keyDown(input, { key });
    expect(input).toHaveValue(expected);
    expect(onCommit).toHaveBeenLastCalledWith(expected);
  });
});
