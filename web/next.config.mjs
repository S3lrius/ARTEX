import { fileURLToPath } from "node:url";

// Static export: `NEXT_EXPORT=1 next build` writes a static site to web/out,
// ready to drop into an nginx web root. `next dev` leaves this unset and keeps
// the /api proxy and hot reload.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: the whole site uses mock data, so there is no backend and no /api proxy.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // Keep a parent lockfile from changing root inference and asset paths.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // Allow LAN IPs to load dev assets (HMR). Add or remove entries as needed.
  // In dev, any IPv4 origin may reach /_next/* and HMR, so a changing LAN address still works.
  // Next forbids a bare "*" for safety, so this uses a segmented wildcard: "*.*.*.*" matches any IPv4.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // Pure static export: no Node runtime, images are not optimized, each route is <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo: no backend, so no /api proxy.
          images: { unoptimized: true },
        }
      : {
          // Dev: proxy /api/* to the Go backend (default :8787, override with AUTOPENTEST_API).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
