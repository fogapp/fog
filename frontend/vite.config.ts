import { defineConfig } from "vite";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// Builds into internal/ui/dist/app so go:embed picks it up. The app/
// subdirectory (gitignored) is what emptyOutDir wipes; the tracked
// placeholder at dist/index.html is never touched by a build.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  // The chat SDK is source-distributed from clients/chat-sdk and bundled
  // here; a standalone app aliases it the same way.
  resolve: {
    alias: { "@fog/chat-sdk": fileURLToPath(new URL("../clients/chat-sdk/src/index.ts", import.meta.url)) },
  },
  build: {
    outDir: "../internal/ui/dist/app",
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: process.env.API_TARGET || "http://localhost:8080",
        changeOrigin: true,
        ws: true,
        // fogd rejects mutating /api requests whose Origin doesn't match
        // its own host. changeOrigin only rewrites Host, so a browser on
        // the dev port would 403 on every POST; send the target as Origin.
        configure: (proxy, options) => {
          proxy.on("proxyReq", (proxyReq, req) => {
            if (req.headers.origin) proxyReq.setHeader("origin", String(options.target));
          });
        },
      },
    },
  },
});
