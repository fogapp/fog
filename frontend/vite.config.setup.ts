import { defineConfig, type Plugin } from "vite";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The files the wizard takes from the dashboard's public/: the icons
// setup/index.html links and the mark Logo masks. The rest of public/ — the
// chat service worker, store logos, PWA images, fonts the CSS already
// bundles — has no business in the fog-setup binary, so the directory is
// not the wizard's publicDir; these are served in dev and emitted at build.
const SETUP_PUBLIC: Record<string, string> = {
  "favicon.svg": "image/svg+xml",
  "favicon.ico": "image/x-icon",
  "apple-touch-icon.png": "image/png",
  "fog.png": "image/png",
  // The pre-paint theme script: a file, since the CSP allows no inline script.
  "theme-boot.js": "text/javascript",
};

function setupPublic(): Plugin {
  const dir = fileURLToPath(new URL("./public/", import.meta.url));
  return {
    name: "fog-setup-public",
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const name = (req.url ?? "").split("?")[0].slice(1);
        const type = SETUP_PUBLIC[name];
        if (!type) return next();
        res.setHeader("Content-Type", type);
        res.end(readFileSync(dir + name));
      });
    },
    generateBundle() {
      for (const name of Object.keys(SETUP_PUBLIC)) {
        this.emitFile({ type: "asset", fileName: name, source: readFileSync(dir + name) });
      }
    },
  };
}

// The installer wizard: a second, separately embedded app sharing the
// dashboard's design system. Builds into internal/setupui/dist/app so
// go:embed bakes it into the fog-setup binary; the app/ subdirectory
// (gitignored) is what emptyOutDir wipes, so the tracked placeholder at
// dist/index.html is never touched by a build.
export default defineConfig({
  plugins: [react(), tailwindcss(), setupPublic()],
  root: "setup",
  publicDir: false,
  build: {
    outDir: "../../internal/setupui/dist/app",
    emptyOutDir: true,
  },
  server: {
    port: 5174,
    proxy: {
      "/api": {
        target: process.env.API_TARGET || "http://localhost:8090",
        changeOrigin: true,
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
