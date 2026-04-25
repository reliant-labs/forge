import React from "react";

interface SparklinePoint {
  value: number;
}

interface MetricCardProps {
  label: string;
  value: string | number;
  change?: { value: number; label?: string };
  icon?: React.ReactNode;
  sparkline?: SparklinePoint[];
  href?: string;
}

function Sparkline({ points }: { points: SparklinePoint[] }) {
  if (points.length < 2) return null;

  const width = 80;
  const height = 32;
  const padding = 2;

  const values = points.map((p) => p.value);
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;

  const pathPoints = points.map((p, i) => {
    const x = padding + (i / (points.length - 1)) * (width - padding * 2);
    const y = height - padding - ((p.value - min) / range) * (height - padding * 2);
    return `${x},${y}`;
  });

  const trend = values[values.length - 1] >= values[0];

  return (
    <svg width={width} height={height} className="flex-shrink-0">
      <polyline
        points={pathPoints.join(" ")}
        fill="none"
        stroke={trend ? "#22c55e" : "#ef4444"}
        strokeWidth={1.5}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

function TrendIndicator({ change }: { change: { value: number; label?: string } }) {
  const isPositive = change.value > 0;
  const isNeutral = change.value === 0;

  return (
    <div
      className={`inline-flex items-center gap-0.5 rounded-full px-2 py-0.5 text-xs font-medium ${
        isNeutral
          ? "bg-gray-100 text-gray-600"
          : isPositive
            ? "bg-green-50 text-green-700"
            : "bg-red-50 text-red-700"
      }`}
    >
      {!isNeutral && (
        <svg
          className={`h-3 w-3 ${isPositive ? "" : "rotate-180"}`}
          fill="none"
          viewBox="0 0 24 24"
          strokeWidth={2.5}
          stroke="currentColor"
        >
          <path strokeLinecap="round" strokeLinejoin="round" d="M4.5 19.5l15-15m0 0H8.25m11.25 0v11.25" />
        </svg>
      )}
      {isPositive ? "+" : ""}
      {change.value}%{change.label && <span className="ml-0.5 text-gray-500">{change.label}</span>}
    </div>
  );
}

export default function MetricCard({ label, value, change, icon, sparkline, href }: MetricCardProps) {
  const content = (
    <div className="rounded-lg border border-gray-200 bg-white p-5 shadow-sm transition-shadow hover:shadow-md">
      <div className="flex items-start justify-between">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            {icon && <div className="flex-shrink-0 text-gray-400">{icon}</div>}
            <p className="text-sm font-medium text-gray-500 truncate">{label}</p>
          </div>
          <div className="mt-2 flex items-baseline gap-2">
            <p className="text-2xl font-semibold text-gray-900">{value}</p>
            {change && <TrendIndicator change={change} />}
          </div>
        </div>
        {sparkline && <Sparkline points={sparkline} />}
      </div>
    </div>
  );

  if (href) {
    return (
      <a href={href} className="block">
        {content}
      </a>
    );
  }

  return content;
}
