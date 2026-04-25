import React, { useEffect, useRef, useState } from "react";

interface MenuItem {
  label: string;
  onClick?: () => void;
  href?: string;
  icon?: React.ReactNode;
  variant?: "default" | "danger";
  disabled?: boolean;
}

interface MenuGroup {
  label?: string;
  items: MenuItem[];
}

interface DropdownMenuProps {
  trigger: React.ReactNode;
  groups: MenuGroup[];
  align?: "left" | "right";
}

export default function DropdownMenu({ trigger, groups, align = "right" }: DropdownMenuProps) {
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
      <div onClick={() => setOpen(!open)} className="cursor-pointer">
        {trigger}
      </div>
      {open && (
        <div
          className={`absolute z-50 mt-1 min-w-[200px] rounded-lg border border-gray-200 bg-white py-1 shadow-lg ${
            align === "right" ? "right-0" : "left-0"
          }`}
        >
          {groups.map((group, gi) => (
            <div key={gi}>
              {gi > 0 && <div className="my-1 border-t border-gray-100" />}
              {group.label && (
                <div className="px-3 py-1.5 text-xs font-semibold uppercase tracking-wider text-gray-400">
                  {group.label}
                </div>
              )}
              {group.items.map((item, ii) => {
                const baseStyle = `flex w-full items-center gap-2 px-3 py-2 text-sm transition-colors ${
                  item.disabled
                    ? "cursor-not-allowed text-gray-300"
                    : item.variant === "danger"
                      ? "text-red-600 hover:bg-red-50"
                      : "text-gray-700 hover:bg-gray-50"
                }`;

                if (item.href) {
                  return (
                    <a key={ii} href={item.href} className={baseStyle} onClick={() => setOpen(false)}>
                      {item.icon && <span className="h-4 w-4">{item.icon}</span>}
                      {item.label}
                    </a>
                  );
                }
                return (
                  <button
                    key={ii}
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
          ))}
        </div>
      )}
    </div>
  );
}
