"use client";

import { useEffect } from "react";

export default function Error({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    // Log to console and telemetry sink. Replace with your own reporter
    // (e.g. OpenTelemetry, Sentry) as needed.
    console.error(error);
  }, [error]);

  return (
    <main className="flex flex-1 flex-col items-center justify-center p-8">
      <div className="max-w-md text-center">
        <h1 className="mb-4 text-3xl font-bold tracking-tight">Something went wrong</h1>
        <p className="mb-6 text-ink-muted">
          An unexpected error occurred. You can try again, or return later.
        </p>
        {error.digest ? (
          <p className="mb-6 font-mono text-xs text-ink-muted">Error ID: {error.digest}</p>
        ) : null}
        <button
          type="button"
          onClick={reset}
          className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent hover:bg-accent-hover"
        >
          Try again
        </button>
      </div>
    </main>
  );
}
