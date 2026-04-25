import React, { useState } from "react";

interface FilterOption {
  label: string;
  value: string;
}

interface FilterDef {
  key: string;
  label: string;
  type: "select" | "search";
  options?: FilterOption[];
  placeholder?: string;
}

interface ActiveFilter {
  key: string;
  label: string;
  value: string;
  displayValue: string;
}

interface FilterBarProps {
  filters: FilterDef[];
  onFilterChange: (filters: Record<string, string>) => void;
  searchPlaceholder?: string;
}

export default function FilterBar({
  filters,
  onFilterChange,
  searchPlaceholder = "Search...",
}: FilterBarProps) {
  const [values, setValues] = useState<Record<string, string>>({});

  function updateFilter(key: string, value: string) {
    const next = { ...values, [key]: value };
    if (!value) delete next[key];
    setValues(next);
    onFilterChange(next);
  }

  function removeFilter(key: string) {
    const next = { ...values };
    delete next[key];
    setValues(next);
    onFilterChange(next);
  }

  function clearAll() {
    setValues({});
    onFilterChange({});
  }

  const activeFilters: ActiveFilter[] = Object.entries(values)
    .filter(([, v]) => v)
    .map(([key, value]) => {
      const def = filters.find((f) => f.key === key);
      const displayValue =
        def?.options?.find((o) => o.value === value)?.label ?? value;
      return { key, label: def?.label ?? key, value, displayValue };
    });

  const searchFilter = filters.find((f) => f.type === "search");
  const selectFilters = filters.filter((f) => f.type === "select");

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-3">
        {/* Search */}
        {searchFilter && (
          <div className="relative min-w-[200px] flex-1">
            <svg
              className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-gray-400"
              fill="none"
              stroke="currentColor"
              viewBox="0 0 24 24"
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                strokeWidth={2}
                d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z"
              />
            </svg>
            <input
              type="text"
              value={values[searchFilter.key] ?? ""}
              onChange={(e) => updateFilter(searchFilter.key, e.target.value)}
              placeholder={searchFilter.placeholder ?? searchPlaceholder}
              className="w-full rounded-lg border border-gray-300 bg-white py-2 pl-9 pr-3 text-sm shadow-sm placeholder:text-gray-400 focus:border-blue-500 focus:outline-none focus:ring-1 focus:ring-blue-500"
            />
          </div>
        )}

        {/* Select Filters */}
        {selectFilters.map((filter) => (
          <select
            key={filter.key}
            value={values[filter.key] ?? ""}
            onChange={(e) => updateFilter(filter.key, e.target.value)}
            className="rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm shadow-sm focus:border-blue-500 focus:outline-none focus:ring-1 focus:ring-blue-500"
          >
            <option value="">{filter.label}</option>
            {filter.options?.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
              </option>
            ))}
          </select>
        ))}
      </div>

      {/* Active Filter Chips */}
      {activeFilters.length > 0 && (
        <div className="flex flex-wrap items-center gap-2">
          {activeFilters.map((f) => (
            <span
              key={f.key}
              className="inline-flex items-center gap-1 rounded-full bg-blue-50 py-1 pl-3 pr-1.5 text-xs font-medium text-blue-700"
            >
              {f.label}: {f.displayValue}
              <button
                onClick={() => removeFilter(f.key)}
                className="flex h-4 w-4 items-center justify-center rounded-full transition hover:bg-blue-200"
              >
                <svg className="h-3 w-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </span>
          ))}
          <button
            onClick={clearAll}
            className="text-xs font-medium text-gray-500 transition hover:text-gray-700"
          >
            Clear all
          </button>
        </div>
      )}
    </div>
  );
}
