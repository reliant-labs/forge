import React from "react";

/**
 * Label — form field label primitive. Renders a native <label> with the
 * forge-default font size/weight + an optional required asterisk. Wrap an
 * <Input>/<Select> as a child, or use `htmlFor` to point at a sibling.
 */
export interface LabelProps
  extends React.LabelHTMLAttributes<HTMLLabelElement> {
  /** When true, append a red "*" after the label text. */
  required?: boolean;
}

export default function Label({
  required,
  className,
  children,
  ...rest
}: LabelProps) {
  const composed = [
    "mb-1 block text-sm font-medium text-gray-700",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <label className={composed} {...rest}>
      {children}
      {required ? (
        <span aria-hidden className="ml-0.5 text-red-500">
          *
        </span>
      ) : null}
    </label>
  );
}
