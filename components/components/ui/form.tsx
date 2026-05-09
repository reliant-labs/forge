import React from "react";

/**
 * Form primitives — minimal, unopinionated wrappers for building a
 * forms-as-react-tree shape. The root <Form> is just a styled <form>;
 * <FormField>, <FormError>, and <FormActions> handle spacing and error
 * presentation so individual pages stop hand-rolling them.
 *
 * For schema validation, pair with `react-hook-form` + `zod` (already a
 * dependency in the auth-ui pack); these primitives don't bundle a
 * validator.
 */
export type FormProps = React.FormHTMLAttributes<HTMLFormElement>;

export default function Form({
  className,
  noValidate,
  children,
  ...rest
}: FormProps) {
  const composed = ["space-y-4", className ?? ""].filter(Boolean).join(" ");
  return (
    <form noValidate={noValidate ?? true} className={composed} {...rest}>
      {children}
    </form>
  );
}

/**
 * FormField — vertical-stack container for a Label + Input + optional
 * error. Use as the building block for each row of a form.
 */
export interface FormFieldProps {
  className?: string;
  children: React.ReactNode;
}

export function FormField({ className, children }: FormFieldProps) {
  const composed = ["block", className ?? ""].filter(Boolean).join(" ");
  return <div className={composed}>{children}</div>;
}

/**
 * FormError — small red message anchored under an input. Renders
 * nothing when `message` is empty/undefined so callers can safely
 * pass `errors.foo?.message` from react-hook-form.
 */
export interface FormErrorProps {
  message?: string | null;
  className?: string;
}

export function FormError({ message, className }: FormErrorProps) {
  if (!message) return null;
  const composed = ["mt-1 text-xs text-red-600", className ?? ""]
    .filter(Boolean)
    .join(" ");
  return <p className={composed}>{message}</p>;
}

/**
 * FormActions — horizontal action row at the bottom of a form
 * (primary submit + optional cancel). Right-aligned by default.
 */
export interface FormActionsProps {
  className?: string;
  align?: "start" | "end" | "between";
  children: React.ReactNode;
}

const alignStyles: Record<NonNullable<FormActionsProps["align"]>, string> = {
  start: "justify-start",
  end: "justify-end",
  between: "justify-between",
};

export function FormActions({
  className,
  align = "end",
  children,
}: FormActionsProps) {
  const composed = [
    "mt-2 flex items-center gap-2",
    alignStyles[align],
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");
  return <div className={composed}>{children}</div>;
}
