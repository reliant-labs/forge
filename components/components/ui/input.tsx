import React from "react";

/**
 * Input — generic text input primitive. Wraps a native <input> with
 * tasteful Tailwind defaults plus optional invalid styling. All standard
 * <input> attributes (type, value, onChange, autoComplete, ...) are
 * forwarded; pass `className` to extend or override.
 *
 * Pair with the <Label> primitive for labelling and accessibility.
 */
export type InputSize = "sm" | "md" | "lg";

export interface InputProps
  extends Omit<React.InputHTMLAttributes<HTMLInputElement>, "size"> {
  /** Visual size of the control. Defaults to "md". */
  inputSize?: InputSize;
  /** Mark the input as invalid; toggles the red focus ring. */
  invalid?: boolean;
}

const sizeStyles: Record<InputSize, string> = {
  sm: "h-8 px-2.5 text-xs",
  md: "h-10 px-3 text-sm",
  lg: "h-12 px-3.5 text-base",
};

const Input = React.forwardRef<HTMLInputElement, InputProps>(function Input(
  { inputSize = "md", invalid, className, type, ...rest },
  ref,
) {
  const composed = [
    "block w-full rounded-md border bg-white shadow-sm",
    "placeholder:text-gray-400",
    "focus:outline-none focus:ring-1",
    "disabled:cursor-not-allowed disabled:bg-gray-50 disabled:text-gray-500",
    invalid
      ? "border-red-400 focus:border-red-500 focus:ring-red-500"
      : "border-gray-300 focus:border-blue-500 focus:ring-blue-500",
    sizeStyles[inputSize],
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");

  return <input ref={ref} type={type ?? "text"} className={composed} {...rest} />;
});

export default Input;
