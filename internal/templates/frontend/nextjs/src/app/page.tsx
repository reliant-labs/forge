export default function Home() {
  return (
    <main className="flex min-h-screen flex-col items-center justify-center p-8">
      <div className="max-w-2xl text-center">
        <h1 className="mb-4 text-4xl font-bold tracking-tight">
          Welcome to Your App
        </h1>
        <p className="mb-8 text-lg text-gray-600">
          Built with Next.js, Connect-ES, and Tailwind CSS.
        </p>
        <div className="flex gap-4 justify-center">
          <a
            href={`${process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'}/healthz`}
            className="rounded-md bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-700"
          >
            Health Check
          </a>
          <a
            href="https://connectrpc.com/docs/web/getting-started"
            target="_blank"
            rel="noopener noreferrer"
            className="rounded-md border border-gray-300 px-4 py-2 text-sm font-medium hover:bg-gray-50"
          >
            Connect-ES Docs
          </a>
        </div>
      </div>
    </main>
  );
}