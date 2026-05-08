import React, { useEffect, useRef, useState } from "react";

export interface RowAction {
  label: string;
  onClick?: () => void;
  href?: string;
  icon?: React.ReactNode;
  variant?: "default" | "danger";
  disabled?: boolean;
}

interface RowActionsMenuProps {
  actions: RowAction[];
  /** Optional menu trigger label for screen readers (defaults to "Row actions"). */
  triggerLabel?: string;
  /** Alignment of the dropdown panel relative to the trigger. */
  align?: "left" | "right";
}

/**
 * RowActionsMenu — kebab trigger plus dropdown of row-scoped actions.
 *
 * Distinct from the lower-level <DropdownMenu/> which is a generic surface
 * with arbitrary trigger and grouped sections. RowActionsMenu is tuned for
 * the repeating "suspend / resume / delete" shape that shows up in every
 * admin list page (workspaces, daemons, api-keys, ...).
 */
export default function RowActionsMenu({ actions, triggerLabel = "Row actions", align = "right" }: RowActionsMenuProps) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    function handleClickOutside(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    }
    if (open) document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, [open]);

  useEffect(() => {
    function handleEsc(e: KeyboardEvent) {
      if (e.key === "Escape") setOpen(false);
    }
    if (open) document.addEventListener("keydown", handleEsc);
    return () => document.removeEventListener("keydown", handleEsc);
  }, [open]);

  return (
    <div className="relative inline-block" ref={ref}>
      <button
        type="button"
        aria-label={triggerLabel}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={(e) => {
          e.stopPropagation();
          setOpen((v) => !v);
        }}
        className="inline-flex h-7 w-7 items-center justify-center rounded-md text-gray-500 hover:bg-gray-100 hover:text-gray-700 focus:outline-none focus:ring-2 focus:ring-blue-500"
      >
        <svg className="h-4 w-4" fill="currentColor" viewBox="0 0 20 20" aria-hidden="true">
          <path d="M10 3a1.5 1.5 0 110 3 1.5 1.5 0 010-3zm0 5.5a1.5 1.5 0 110 3 1.5 1.5 0 010-3zm0 5.5a1.5 1.5 0 110 3 1.5 1.5 0 010-3z" />
        </svg>
      </button>
      {open && (
        <div
          role="menu"
          className={`absolute z-50 mt-1 min-w-[160px] rounded-lg border border-gray-200 bg-white py-1 shadow-lg ${
            align === "right" ? "right-0" : "left-0"
          }`}
        >
          {actions.map((item, ii) => {
            const baseStyle = `flex w-full items-center gap-2 px-3 py-2 text-sm transition-colors ${
              item.disabled
                ? "cursor-not-allowed text-gray-300"
                : item.variant === "danger"
                  ? "text-red-600 hover:bg-red-50"
                  : "text-gray-700 hover:bg-gray-50"
            }`;

            if (item.href) {
              return (
                <a
                  key={ii}
                  role="menuitem"
                  href={item.href}
                  className={baseStyle}
                  onClick={() => setOpen(false)}
                >
                  {item.icon && <span className="h-4 w-4">{item.icon}</span>}
                  {item.label}
                </a>
              );
            }
            return (
              <button
                key={ii}
                type="button"
                role="menuitem"
                onClick={() => {
                  if (!item.disabled) {
                    item.onClick?.();
                    setOpen(false);
                  }
                }}
                disabled={item.disabled}
                className={baseStyle}
              >
                {item.icon && <span className="h-4 w-4">{item.icon}</span>}
                {item.label}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
