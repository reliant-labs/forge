import React from "react";

interface StatCard {
  label: string;
  value: string;
  icon?: React.ReactNode;
  trend?: "up" | "down" | "neutral";
  trendValue?: string;
}

interface StatCardsProps {
  stats: StatCard[];
  columns?: 2 | 3 | 4;
}

const columnClasses: Record<number, string> = {
  2: "grid-cols-1 sm:grid-cols-2",
  3: "grid-cols-1 sm:grid-cols-2 lg:grid-cols-3",
  4: "grid-cols-1 sm:grid-cols-2 lg:grid-cols-4",
};

const trendConfig: Record<string, { color: string; bg: string; arrow: string }> = {
  up: { color: "text-green-700", bg: "bg-green-50", arrow: "↑" },
  down: { color: "text-red-700", bg: "bg-red-50", arrow: "↓" },
  neutral: { color: "text-gray-600", bg: "bg-gray-100", arrow: "→" },
};

export default function StatCards({ stats, columns = 4 }: StatCardsProps) {
  return (
    <div className={`grid gap-4 ${columnClasses[columns]}`}>
      {stats.map((stat) => {
        const trend = stat.trend ? trendConfig[stat.trend] : null;
        return (
          <div
            key={stat.label}
            className="rounded-xl border border-gray-200 bg-white p-5 shadow-sm"
          >
            <div className="flex items-center justify-between">
              <p className="text-sm font-medium text-gray-500">{stat.label}</p>
              {stat.icon && (
                <div className="flex h-9 w-9 items-center justify-center rounded-lg bg-blue-50 text-blue-600">
                  {stat.icon}
                </div>
              )}
            </div>
            <div className="mt-3 flex items-end gap-2">
              <p className="text-2xl font-bold tracking-tight text-gray-900">
                {stat.value}
              </p>
              {trend && stat.trendValue && (
                <span
                  className={`mb-0.5 inline-flex items-center gap-0.5 rounded-full px-2 py-0.5 text-xs font-semibold ${trend.bg} ${trend.color}`}
                >
                  {trend.arrow} {stat.trendValue}
                </span>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}
