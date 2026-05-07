import React from "react";

type StatusDotVariant = "active" | "paused" | "pending" | "error" | "warning" | "neutral";
type StatusDotSize = "sm" | "md" | "lg";

interface StatusDotProps {
  /** Visual variant — controls dot color and label tint. */
  variant?: StatusDotVariant;
  /** Optional textual label rendered alongside the dot. */
  label?: React.ReactNode;
  size?: StatusDotSize;
  /** When true, the dot pulses (useful for "pending" / "syncing" shapes). */
  pulse?: boolean;
  className?: string;
}

const dotTint: Record<StatusDotVariant, string> = {
  active: "bg-green-500",
  paused: "bg-yellow-500",
  pending: "bg-blue-500",
  error: "bg-red-500",
  warning: "bg-amber-500",
  neutral: "bg-gray-400",
};

const labelTint: Record<StatusDotVariant, string> = {
  active: "text-green-700",
  paused: "text-yellow-700",
  pending: "text-blue-700",
  error: "text-red-700",
  warning: "text-amber-700",
  neutral: "text-gray-600",
};

const dotSize: Record<StatusDotSize, string> = {
  sm: "h-1.5 w-1.5",
  md: "h-2 w-2",
  lg: "h-2.5 w-2.5",
};

/**
 * StatusDot — colored dot plus optional label for compact status display.
 *
 * Distinct from <Badge/> (pill-shaped, broader status taxonomy) — StatusDot
 * is the recurring shape in dense list/table cells where space is tight
 * (workspace state, daemon connection state, billing subscription state).
 */
export default function StatusDot({
  variant = "neutral",
  label,
  size = "md",
  pulse,
  className,
}: StatusDotProps) {
  return (
    <span className={`inline-flex items-center gap-1.5 ${className ?? ""}`}>
      <span
        aria-hidden="true"
        className={`inline-block rounded-full ${dotTint[variant]} ${dotSize[size]} ${pulse ? "animate-pulse" : ""}`}
      />
      {label !== undefined && (
        <span className={`text-xs font-medium ${labelTint[variant]}`}>{label}</span>
      )}
    </span>
  );
}
