import React, { useState } from "react";

interface Tab {
  id: string;
  label: string;
  icon?: React.ReactNode;
  badge?: string | number;
  disabled?: boolean;
}

interface TabsProps {
  tabs: Tab[];
  defaultTab?: string;
  onChange?: (tabId: string) => void;
  variant?: "underline" | "pills" | "boxed";
  children?: (activeTab: string) => React.ReactNode;
}

export default function Tabs({ tabs, defaultTab, onChange, variant = "underline", children }: TabsProps) {
  const [active, setActive] = useState(defaultTab ?? tabs[0]?.id ?? "");

  function handleSelect(tabId: string) {
    setActive(tabId);
    onChange?.(tabId);
  }

  const styles = {
    underline: {
      container: "border-b border-gray-200",
      tab: (isActive: boolean, disabled: boolean) =>
        `relative px-4 py-2.5 text-sm font-medium transition-colors ${
          disabled
            ? "cursor-not-allowed text-gray-300"
            : isActive
              ? "text-blue-600"
              : "text-gray-500 hover:text-gray-700"
        }`,
      indicator: "absolute bottom-0 left-0 right-0 h-0.5 bg-blue-600",
    },
    pills: {
      container: "flex gap-1 rounded-lg bg-gray-100 p-1",
      tab: (isActive: boolean, disabled: boolean) =>
        `rounded-md px-3 py-1.5 text-sm font-medium transition-all ${
          disabled
            ? "cursor-not-allowed text-gray-300"
            : isActive
              ? "bg-white text-gray-900 shadow-sm"
              : "text-gray-600 hover:text-gray-800"
        }`,
      indicator: "",
    },
    boxed: {
      container: "flex gap-0 rounded-lg border border-gray-200 overflow-hidden",
      tab: (isActive: boolean, disabled: boolean) =>
        `px-4 py-2 text-sm font-medium border-r border-gray-200 last:border-r-0 transition-colors ${
          disabled
            ? "cursor-not-allowed text-gray-300 bg-gray-50"
            : isActive
              ? "bg-blue-50 text-blue-700"
              : "bg-white text-gray-600 hover:bg-gray-50"
        }`,
      indicator: "",
    },
  };

  const s = styles[variant];

  return (
    <div>
      <div className={s.container} role="tablist">
        {tabs.map((tab) => {
          const isActive = tab.id === active;
          return (
            <button
              key={tab.id}
              role="tab"
              aria-selected={isActive}
              disabled={tab.disabled}
              onClick={() => !tab.disabled && handleSelect(tab.id)}
              className={s.tab(isActive, !!tab.disabled)}
            >
              <span className="flex items-center gap-1.5">
                {tab.icon}
                {tab.label}
                {tab.badge !== undefined && (
                  <span className={`rounded-full px-1.5 py-0.5 text-[10px] font-semibold ${
                    isActive ? "bg-blue-100 text-blue-600" : "bg-gray-200 text-gray-600"
                  }`}>
                    {tab.badge}
                  </span>
                )}
              </span>
              {variant === "underline" && isActive && <div className={s.indicator} />}
            </button>
          );
        })}
      </div>
      {children && <div className="mt-4" role="tabpanel">{children(active)}</div>}
    </div>
  );
}
