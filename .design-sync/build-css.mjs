// Compiles fog's Tailwind stylesheet for the design-system sync.
//
// fog is an app, not a library: its only stylesheet is the Vite build's
// content-scanned output, which carries exactly the utilities the app's
// source uses and lands under a hashed name outside frontend/. The sync needs
// a stable path inside the package (cfg.cssEntry) and a wider utility set,
// because the design agent writes its own layout glue with fog's vocabulary
// and a class the app never happened to use would otherwise silently do
// nothing. This runs the same compiler the Vite plugin runs, over the same
// source, plus an explicit safelist of layout/typography/color utilities.
//
// Run from the repo root: node .design-sync/build-css.mjs
import { createRequire } from "node:module";
import { readFileSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repo = resolve(fileURLToPath(import.meta.url), "../..");
const frontend = resolve(repo, "frontend");
const base = resolve(frontend, "src");
const req = createRequire(resolve(frontend, "package.json"));
const { compile } = await import(req.resolve("@tailwindcss/node"));
const { Scanner } = await import(req.resolve("@tailwindcss/oxide"));

const safelist = readFileSync(resolve(repo, ".design-sync/safelist.css"), "utf8");
const css = `@import "./index.css";\n@source "./";\n${safelist}`;

const compiler = await compile(css, { base, onDependency: () => {} });
const roots =
  compiler.root === "none" ? [] :
  compiler.root === null ? [{ base, pattern: "**/*", negated: false }] :
  [{ ...compiler.root, negated: false }];
const scanner = new Scanner({ sources: roots.concat(compiler.sources) });
const out = compiler.build(scanner.scan());
const target = resolve(frontend, ".ds-styles.css");
writeFileSync(target, out);
console.error(`wrote ${target} (${out.length} bytes)`);
