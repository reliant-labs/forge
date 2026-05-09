import React from "react";

/**
 * Table — bare structural primitives for building data tables. Each
 * subcomponent wraps the corresponding native table element and applies
 * forge's default border/spacing/typography rules. The default export is
 * the outer <Table> shell (rounded border + horizontal scroll); compose
 * with TableHeader / TableBody / TableRow / TableCell / TableHead for the
 * full tree.
 *
 * Pairs with `@tanstack/react-table` (headless engine) — see the
 * `data-table` pack for the canonical usage. The pack consumes these
 * primitives instead of inlining `<table>` markup.
 */
export interface TableProps extends React.HTMLAttributes<HTMLTableElement> {
  /** Wrap in a bordered container with overflow-x. Defaults to true. */
  bordered?: boolean;
}

function Table({
  bordered = true,
  className,
  children,
  ...rest
}: TableProps) {
  const tableClass = [
    "min-w-full divide-y divide-gray-200",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");

  if (!bordered) {
    return (
      <table className={tableClass} {...rest}>
        {children}
      </table>
    );
  }

  return (
    <div className="overflow-x-auto rounded-lg border border-gray-200 bg-white shadow-sm">
      <table className={tableClass} {...rest}>
        {children}
      </table>
    </div>
  );
}

export default Table;

export type TableHeaderProps = React.HTMLAttributes<HTMLTableSectionElement>;

export function TableHeader({
  className,
  children,
  ...rest
}: TableHeaderProps) {
  const composed = ["bg-gray-50", className ?? ""].filter(Boolean).join(" ");
  return (
    <thead className={composed} {...rest}>
      {children}
    </thead>
  );
}

export type TableBodyProps = React.HTMLAttributes<HTMLTableSectionElement>;

export function TableBody({ className, children, ...rest }: TableBodyProps) {
  const composed = ["divide-y divide-gray-200", className ?? ""]
    .filter(Boolean)
    .join(" ");
  return (
    <tbody className={composed} {...rest}>
      {children}
    </tbody>
  );
}

export interface TableRowProps
  extends React.HTMLAttributes<HTMLTableRowElement> {
  /** Striped (alternating row) styling. Caller passes the row index. */
  striped?: boolean;
  /** Render as clickable; adds hover/cursor styling. */
  clickable?: boolean;
}

export function TableRow({
  striped,
  clickable,
  className,
  children,
  ...rest
}: TableRowProps) {
  const composed = [
    "transition-colors",
    clickable ? "cursor-pointer hover:bg-blue-50" : "",
    striped ? "bg-gray-50" : "bg-white",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <tr className={composed} {...rest}>
      {children}
    </tr>
  );
}

export interface TableHeadProps
  extends React.ThHTMLAttributes<HTMLTableCellElement> {
  /** Mark the header cell as sortable; flips the cursor. */
  sortable?: boolean;
}

export function TableHead({
  sortable,
  className,
  children,
  scope,
  ...rest
}: TableHeadProps) {
  const composed = [
    "px-4 py-3 text-left text-xs font-semibold uppercase tracking-wider text-gray-500",
    sortable ? "cursor-pointer select-none" : "",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <th scope={scope ?? "col"} className={composed} {...rest}>
      {children}
    </th>
  );
}

export type TableCellProps = React.TdHTMLAttributes<HTMLTableCellElement>;

export function TableCell({
  className,
  children,
  ...rest
}: TableCellProps) {
  const composed = [
    "whitespace-nowrap px-4 py-3 text-sm text-gray-700",
    className ?? "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <td className={composed} {...rest}>
      {children}
    </td>
  );
}
