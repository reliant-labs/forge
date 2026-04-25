"use client";

import { useEffect } from "react";

export default function GlobalError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error(error);
  }, [error]);

  return (
    <html lang="en">
      <body className="font-sans antialiased">
        <main className="flex flex-1 flex-col items-center justify-center p-8">
          <div className="max-w-md text-center">
            <h1 className="mb-4 text-3xl font-bold tracking-tight">
              Application error
            </h1>
            <p className="mb-6 text-gray-600">
              A critical error occurred and the app could not recover.
            </p>
            {error.digest ? (
              <p className="mb-6 font-mono text-xs text-gray-500">
                Error ID: {error.digest}
              </p>
            ) : null}
            <button
              type="button"
              onClick={reset}
              className="rounded-md bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-700"
            >
              Try again
            </button>
          </div>
        </main>
      </body>
    </html>
  );
}