import Link from "next/link";

export default function NotFound() {
  return (
    <div className="flex min-h-screen flex-col items-center justify-center gap-4 px-6 text-center">
      <div className="font-mono text-5xl text-brass">404</div>
      <h1 className="font-display text-xl font-bold text-ink-hi">Nothing on this frequency</h1>
      <p className="max-w-sm text-sm text-ink-mid">The resource you're looking for doesn't exist or has moved.</p>
      <Link href="/" className="btn-brass mt-2">
        ← Back to Overview
      </Link>
    </div>
  );
}
