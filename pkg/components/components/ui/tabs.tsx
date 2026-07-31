import React, { useState } from "react";

interface Tab {
  id: string;
  label: string;
  icon?: React.ReactNode;
  badge?: string | number;
  disabled?: boolean;
}

/**
 * Tabs — CONTROLLED or uncontrolled, the standard React pair.
 *
 * Pass `activeTab` and the parent owns the selection; omit it and the
 * component keeps its own, seeded from `defaultTab`. The controlled half is
 * not a nicety: forge's own generated list pages read their filters from the
 * URL (`useTypedSearchParams`), so a tab bar that owns its state cannot
 * participate in the convention forge ships — the URL and the bar disagree the
 * moment anyone deep-links, refreshes, or hits Back. A page hitting that
 * hand-rolls a tab bar, which is how a component library loses its reason to
 * exist.
 *
 *   const [params, setParams] = useTypedSearchParams(schema);
 *   <Tabs tabs={tabs} activeTab={params.status}
 *         onChange={(id) => setParams({ ...params, status: id })} />
 */
interface TabsProps {
  tabs: Tab[];
  /**
   * Controlled selection. When set, the component renders exactly this and
   * NEVER self-updates — clicking calls `onChange` and nothing moves until
   * the parent says so, which is what makes the URL the single truth.
   */
  activeTab?: string;
  /** Initial selection when uncontrolled. Ignored if `activeTab` is set. */
  defaultTab?: string;
  onChange?: (tabId: string) => void;
  variant?: "underline" | "pills" | "boxed";
  children?: (activeTab: string) => React.ReactNode;
}

export default function Tabs({ tabs, activeTab, defaultTab, onChange, variant = "underline", children }: TabsProps) {
  const [uncontrolled, setUncontrolled] = useState(defaultTab ?? tabs[0]?.id ?? "");
  const controlled = activeTab !== undefined;
  const active = activeTab ?? uncontrolled;

  function handleSelect(tabId: string) {
    if (!controlled) setUncontrolled(tabId);
    onChange?.(tabId);
  }

  const styles = {
    underline: {
      container: "border-b border-border",
      tab: (isActive: boolean, disabled: boolean) =>
        `relative px-4 py-2.5 text-sm font-medium transition-colors ${
          disabled
            ? "cursor-not-allowed text-ink-subtle"
            : isActive
              ? "text-accent"
              : "text-ink-muted hover:text-ink"
        }`,
      indicator: "absolute bottom-0 left-0 right-0 h-0.5 bg-accent",
    },
    pills: {
      container: "flex gap-1 rounded-lg bg-surface-muted p-1",
      tab: (isActive: boolean, disabled: boolean) =>
        `rounded-md px-3 py-1.5 text-sm font-medium transition-all ${
          disabled
            ? "cursor-not-allowed text-ink-subtle"
            : isActive
              ? "bg-surface text-ink shadow-sm"
              : "text-ink-muted hover:text-ink"
        }`,
      indicator: "",
    },
    boxed: {
      container: "flex gap-0 rounded-lg border border-border overflow-hidden",
      tab: (isActive: boolean, disabled: boolean) =>
        `px-4 py-2 text-sm font-medium border-r border-border last:border-r-0 transition-colors ${
          disabled
            ? "cursor-not-allowed text-ink-subtle bg-surface-muted"
            : isActive
              ? "bg-accent-surface text-accent-ink"
              : "bg-surface text-ink-muted hover:bg-surface-muted"
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
                    isActive ? "bg-accent-surface text-accent-ink" : "bg-border text-ink-muted"
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
