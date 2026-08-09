"use client";

/**
 * Last-resort boundary for errors thrown in the root layout itself (must render
 * its own <html>/<body>). Kept minimal and dependency-free.
 */
export default function GlobalError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  return (
    <html lang="en">
      <body
        style={{
          margin: 0,
          minHeight: "100vh",
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          background: "#0B0C0E",
          color: "#ECEDEF",
          fontFamily: "ui-sans-serif, system-ui, sans-serif",
        }}
      >
        <div style={{ maxWidth: 420, padding: 32, textAlign: "center" }}>
          <div style={{ color: "#E5675B", fontSize: 28, marginBottom: 12 }}>⚠</div>
          <h1 style={{ fontSize: 20, fontWeight: 700, margin: 0 }}>Application error</h1>
          <p style={{ color: "#A0A4AB", fontSize: 14, marginTop: 8 }}>
            The app hit an unexpected error. Please try again.
          </p>
          {error.digest && (
            <p style={{ color: "#6B7079", fontFamily: "ui-monospace, monospace", fontSize: 11, marginTop: 8 }}>
              ref {error.digest}
            </p>
          )}
          <button
            onClick={reset}
            style={{
              marginTop: 20,
              padding: "8px 16px",
              borderRadius: 6,
              border: "1px solid rgba(224,163,64,0.6)",
              background: "rgba(224,163,64,0.1)",
              color: "#E0A340",
              fontWeight: 600,
              cursor: "pointer",
            }}
          >
            Retry
          </button>
        </div>
      </body>
    </html>
  );
}
