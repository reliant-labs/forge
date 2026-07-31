import React from "react";

interface DashboardOpsProps {
  /** Page title. */
  title: string;
  /** Header right content — incident severity selector, refresh control, etc. */
  headerActions?: React.ReactNode;
  /** Alerts panel — typically a list of active incidents or warnings. Rendered left. */
  alerts: React.ReactNode;
  /** Status grid — system / service health tiles. Rendered center, widest. */
  statusGrid: React.ReactNode;
  /** Log stream — chronological event feed. Rendered right. */
  logStream: React.ReactNode;
}

/**
 * Ops / SRE dashboard layout. Three columns scroll independently so an
 * operator can keep alerts visible while paging through logs.
 *
 * On screens narrower than `lg`, the three regions stack vertically.
 */
export default function DashboardOps({
  title,
  headerActions,
  alerts,
  statusGrid,
  logStream,
}: DashboardOpsProps) {
  return (
    <div className="flex h-screen w-full flex-col bg-gray-50">
      <header className="flex shrink-0 items-center justify-between border-b border-gray-200 bg-white px-6 py-4">
        <h1 className="text-lg font-semibold tracking-tight text-gray-900">
          {title}
        </h1>
        {headerActions && <div className="flex items-center gap-2">{headerActions}</div>}
      </header>

      <div className="grid min-h-0 flex-1 grid-cols-1 gap-px bg-gray-200 lg:grid-cols-[minmax(280px,1fr)_minmax(0,2fr)_minmax(280px,1fr)]">
        <section
          aria-label="Active alerts"
          className="flex min-h-0 flex-col overflow-hidden bg-white"
        >
          <div className="shrink-0 border-b border-gray-200 px-4 py-2 text-xs font-semibold uppercase tracking-wider text-gray-500">
            Alerts
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto">{alerts}</div>
        </section>

        <section
          aria-label="System status"
          className="flex min-h-0 flex-col overflow-hidden bg-white"
        >
          <div className="shrink-0 border-b border-gray-200 px-4 py-2 text-xs font-semibold uppercase tracking-wider text-gray-500">
            Status
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto p-4">{statusGrid}</div>
        </section>

        <section
          aria-label="Log stream"
          className="flex min-h-0 flex-col overflow-hidden bg-white"
        >
          <div className="shrink-0 border-b border-gray-200 px-4 py-2 text-xs font-semibold uppercase tracking-wider text-gray-500">
            Logs
          </div>
          <div className="min-h-0 flex-1 overflow-y-auto font-mono text-xs">
            {logStream}
          </div>
        </section>
      </div>
    </div>
  );
}
