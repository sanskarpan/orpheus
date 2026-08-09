/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  // The app is a BFF: it talks to the Go API server-side only. The base URL is
  // read from the environment at request time (see lib/orpheus.ts).
  eslint: { ignoreDuringBuilds: true },
  // better-sqlite3 is a native module — keep it external so webpack doesn't try
  // to bundle the .node binary into the server build.
  experimental: {
    serverComponentsExternalPackages: ["better-sqlite3"],
  },
};

export default nextConfig;
