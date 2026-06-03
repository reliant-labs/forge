import React from "react";

// Canonical variants. `danger` and `default` are accepted aliases for
// `error` and `neutral` respectively — many existing codebases (and
// alternate design systems) name the destructive variant `danger` and
// the no-color variant `default`. Accepting both spellings means
// ports don't need an adapter table at every call site.
type BadgeVariant = "success" | "warning" | "error" | "info" | "neutral";
type BadgeVariantAlias = "danger" | "default";
type BadgeSize = "sm" | "md" | "lg";

interface BadgeProps {
  label: string;
  variant?: BadgeVariant | BadgeVariantAlias;
  size?: BadgeSize;
  dot?: boolean;
  removable?: boolean;
  onRemove?: () => void;
}

const variantStyles: Record<BadgeVariant, string> = {
  success: "bg-green-50 text-green-700 ring-green-600/20",
  warning: "bg-yellow-50 text-yellow-800 ring-yellow-600/20",
  error: "bg-red-50 text-red-700 ring-red-600/20",
  info: "bg-blue-50 text-blue-700 ring-blue-600/20",
  neutral: "bg-gray-50 text-gray-600 ring-gray-500/20",
};

const dotStyles: Record<BadgeVariant, string> = {
  success: "bg-green-500",
  warning: "bg-yellow-500",
  error: "bg-red-500",
  info: "bg-blue-500",
  neutral: "bg-gray-500",
};

const sizeStyles: Record<BadgeSize, string> = {
  sm: "px-1.5 py-0.5 text-[11px]",
  md: "px-2 py-0.5 text-xs",
  lg: "px-2.5 py-1 text-sm",
};

function resolveVariant(v: BadgeVariant | BadgeVariantAlias): BadgeVariant {
  if (v === "danger") return "error";
  if (v === "default") return "neutral";
  return v;
}

export default function Badge({ label, variant = "neutral", size = "md", dot, removable, onRemove }: BadgeProps) {
  const v = resolveVariant(variant);
  return (
    <span
      className={`inline-flex items-center gap-1 rounded-full font-medium ring-1 ring-inset ${variantStyles[v]} ${sizeStyles[size]}`}
    >
      {dot && <span className={`h-1.5 w-1.5 rounded-full ${dotStyles[v]}`} />}
      {label}
      {removable && (
        <button
          onClick={onRemove}
          className="ml-0.5 inline-flex h-3.5 w-3.5 items-center justify-center rounded-full hover:bg-black/10"
        >
          <svg className="h-2.5 w-2.5" fill="none" viewBox="0 0 24 24" strokeWidth={3} stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
      )}
    </span>
  );
}
