import {
  useRef,
  useState,
  type ComponentProps,
  type ComponentType,
} from "react";
import { Input } from "@multica/ui/components/ui/input";

/**
 * A number field that keeps transient text separate from its committed value.
 * Clearing is allowed while typing; blur restores an empty field or clamps an
 * out-of-range value. This lets a two-digit entry pass through an invalid first
 * digit without committing a schedule the user never chose.
 */
export function NumberField({
  value,
  min,
  max,
  onCommit,
  className,
  ariaLabel,
  autoFocus,
  component: Field = Input,
}: {
  value: number;
  min: number;
  max: number;
  onCommit: (value: number) => void;
  className?: string;
  ariaLabel: string;
  autoFocus?: boolean;
  component?: ComponentType<ComponentProps<"input">>;
}) {
  const [text, setText] = useState(String(value));
  const lastValueRef = useRef(value);
  if (lastValueRef.current !== value) {
    lastValueRef.current = value;
    setText(String(value));
  }

  return (
    <Field
      type="number"
      aria-label={ariaLabel}
      // Caller-gated: focus only when a user action revealed the field.
      autoFocus={autoFocus}
      min={min}
      max={max}
      value={text}
      onChange={(event) => {
        setText(event.target.value);
        const next = Number.parseInt(event.target.value, 10);
        if (!Number.isNaN(next) && next >= min && next <= max) onCommit(next);
      }}
      onKeyDown={(event) => {
        if (event.key !== "ArrowUp" && event.key !== "ArrowDown") return;
        event.preventDefault();
        const typed = Number.parseInt(text, 10);
        // Values already outside the range land on the bound they overshot.
        // Values at a bound wrap, matching the time fields in this editor.
        const stepped =
          Number.isNaN(typed) || (typed >= min && typed <= max)
            ? (Number.isNaN(typed) ? value : typed) + (event.key === "ArrowUp" ? 1 : -1)
            : Math.min(max, Math.max(min, typed));
        const wrapped = stepped > max ? min : stepped < min ? max : stepped;
        setText(String(wrapped));
        if (wrapped !== lastValueRef.current) onCommit(wrapped);
      }}
      onBlur={() => {
        const typed = Number.parseInt(text, 10);
        if (Number.isNaN(typed)) {
          setText(String(lastValueRef.current));
          return;
        }
        const clamped = Math.min(max, Math.max(min, typed));
        setText(String(clamped));
        if (clamped !== lastValueRef.current) onCommit(clamped);
      }}
      className={className}
    />
  );
}
