import React from "react";

/**
 * Button — generic action primitive used across the app and by frontend
 * packs. Tasteful Tailwind defaults; not a full design system.
 *
 * Variants: primary | secondary | outline | ghost | danger.
 * Sizes:    sm | md | lg.
 *
 * Standard <button> attributes (type, disabled, onClick, aria-*) are
 * forwarded; pass `className` to extend or override the defaults.
 */
export type ButtonVariant =
  | "primary"
  | "secondary"
  | "outline"
  | "ghost"
  | "danger";
export type ButtonSize = "sm" | "md" | "lg";

export interface ButtonProps
  extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  fullWidth?: boolean;
  isLoading?: boolean;
}

const variantStyles: Record<ButtonVariant, string> = {
  primary:
    "bg-blue-600 text-white shadow-sm hover:bg-blue-700 disabled:hover:bg-blue-600",
  secondary:
    "bg-gray-100 text-gray-900 shadow-sm hover:bg-gray-200 disabled:hover:bg-gray-100",
  outline:
    "border border-gray-300 bg-white text-gray-700 shadow-sm hover:bg-gray-50 disabled:hover:bg-white",
  ghost:
    "bg-transparent text-gray-700 hover:bg-gray-100 disabled:hover:bg-transparent",
  danger:
    "bg-red-600 text-white shadow-sm hover:bg-red-700 disabled:hover:bg-red-600",
};

const sizeStyles: Record<ButtonSize, string> = {
  sm: "h-8 px-3 text-xs",
  md: "h-10 px-4 text-sm",
  lg: "h-12 px-6 text-base",
};

export default function Button({
  variant = "primary",
  size = "md",
  fullWidth,
  isLoading,
  disabled,
  className,
  type,
  children,
  ...rest
}: ButtonProps) {
  const composed = [
    "inline-flex items-center justify-center gap-2 rounded-md font-medium transition-colors",
    "focus:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 focus-visible:ring-offset-1",
    "disabled:cursor-not-allowed disabled:opacity-60",
    variantStyles[variant],
    sizeStyles[size],
    fullWidth ? "w-full" : "",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <button
      type={type ?? "button"}
      disabled={disabled || isLoading}
      className={composed}
      {...rest}
    >
      {isLoading ? (
        <span
          aria-hidden
          className="inline-block h-3.5 w-3.5 animate-spin rounded-full border-2 border-current border-t-transparent"
        />
      ) : null}
      {children}
    </button>
  );
}
