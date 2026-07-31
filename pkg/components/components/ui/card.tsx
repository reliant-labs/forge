import React from "react";

/**
 * Card — generic surface primitive. A bordered, rounded white panel with
 * optional padding. Compose with `<CardHeader>`, `<CardBody>`,
 * `<CardFooter>` for the common shapes, or pass children directly for a
 * raw surface.
 *
 * Distinct from `<MetricCard>` / `<StatGrid>` (which are layout-bound
 * domain components); this is the bare building block.
 */
export type CardPadding = "none" | "sm" | "md" | "lg";

export interface CardProps extends React.HTMLAttributes<HTMLDivElement> {
  /** Inner padding. Defaults to "md". */
  padding?: CardPadding;
  /** Render with a hover lift — useful for clickable cards. */
  interactive?: boolean;
}

const paddingStyles: Record<CardPadding, string> = {
  none: "",
  sm: "p-3",
  md: "p-4",
  lg: "p-6",
};

export default function Card({
  padding = "md",
  interactive,
  className,
  children,
  ...rest
}: CardProps) {
  const composed = [
    "rounded-lg border border-border bg-surface shadow-sm",
    paddingStyles[padding],
    interactive
      ? "transition-shadow hover:shadow-md focus-within:shadow-md"
      : "",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <div className={composed} {...rest}>
      {children}
    </div>
  );
}

/**
 * CardHeader — top section of a card with optional title/description.
 * Pass `actions` for a right-aligned action cluster.
 */
export interface CardHeaderProps {
  title?: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
  children?: React.ReactNode;
}

export function CardHeader({
  title,
  description,
  actions,
  className,
  children,
}: CardHeaderProps) {
  const composed = [
    "flex items-start justify-between gap-3 border-b border-border pb-3 mb-3",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <div className={composed}>
      <div>
        {title ? (
          <div className="text-sm font-semibold text-ink">{title}</div>
        ) : null}
        {description ? (
          <div className="mt-0.5 text-xs text-ink-muted">{description}</div>
        ) : null}
        {children}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  );
}

export interface CardBodyProps {
  className?: string;
  children: React.ReactNode;
}

export function CardBody({ className, children }: CardBodyProps) {
  const composed = ["text-sm text-ink", className ?? ""]
    .filter(Boolean)
    .join(" ");
  return <div className={composed}>{children}</div>;
}

export interface CardFooterProps {
  className?: string;
  children: React.ReactNode;
}

export function CardFooter({ className, children }: CardFooterProps) {
  const composed = [
    "mt-3 flex items-center justify-end gap-2 border-t border-border pt-3",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");
  return <div className={composed}>{children}</div>;
}
