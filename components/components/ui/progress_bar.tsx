import React from "react";

type ProgressVariant = "default" | "success" | "warning" | "danger";
type ProgressSize = "sm" | "md" | "lg";

interface ProgressBarProps {
  /** Current value (clamped to [0, max]). */
  value: number;
  /** Maximum value. Defaults to 100 (treat value as a percentage). */
  max?: number;
  /** Optional label rendered above the bar. */
  label?: React.ReactNode;
  /** When true, render the numeric value/max on the right of the label row. */
  showValue?: boolean;
  /** Force a variant. When omitted, the bar auto-tints red >80% (quota-shape default). */
  variant?: ProgressVariant;
  size?: ProgressSize;
  className?: string;
}

const trackSize: Record<ProgressSize, string> = {
  sm: "h-1.5",
  md: "h-2",
  lg: "h-3",
};

const barTint: Record<ProgressVariant, string> = {
  default: "bg-blue-500",
  success: "bg-green-500",
  warning: "bg-yellow-500",
  danger: "bg-red-500",
};

/**
 * ProgressBar — value/max bar with optional label and auto-warning tint.
 *
 * Used in billing usage / quota / capacity displays. The default variant
 * auto-shifts to "danger" tint when fill exceeds 80% so the consumer
 * doesn't have to thread state through.
 */
export default function ProgressBar({
  value,
  max = 100,
  label,
  showValue,
  variant,
  size = "md",
  className,
}: ProgressBarProps) {
  const clampedMax = max <= 0 ? 1 : max;
  const clampedValue = Math.max(0, Math.min(value, clampedMax));
  const pct = (clampedValue / clampedMax) * 100;

  const resolvedVariant: ProgressVariant = variant ?? (pct > 80 ? "danger" : "default");

  return (
    <div className={className}>
      {(label || showValue) && (
        <div className="mb-1 flex items-baseline justify-between text-xs text-gray-600">
          {label && <span>{label}</span>}
          {showValue && (
            <span className="tabular-nums text-gray-500">
              {clampedValue}/{clampedMax}
            </span>
          )}
        </div>
      )}
      <div
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={clampedMax}
        aria-valuenow={clampedValue}
        className={`w-full overflow-hidden rounded-full bg-gray-200 ${trackSize[size]}`}
      >
        <div
          className={`h-full rounded-full transition-[width] duration-300 ${barTint[resolvedVariant]}`}
          style={{ width: `${pct}%` }}
        />
      </div>
    </div>
  );
}
