import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { post, type AppImportResult, type AppSpec } from "../api";
import { appsKey } from "../lib/useApps";
import Field, { controlArea } from "../components/Field";
import SpecEditor, { draftFromSpec, draftToPayload, emptyDraft, type SpecDraft } from "../components/SpecEditor";
import PageHeader from "../components/PageHeader";
import Tabs, { useTabParam } from "../components/Tabs";
import { errorText } from "../components/Async";
import { validateGitRepo } from "../lib/gitUrl";
import { paths } from "../lib/paths";
import Store from "./store/Store";
import { useAction } from "../lib/useAction";

interface ImportedService {
  name: string;
  spec: AppSpec;
  warnings: string[];
}

// Add app has three doors, as peers: the curated store, your own git
// repository, and a container image (or a docker-compose file to convert).
// The store is the default because it is the one-click path; the other two
// are how the operator's own apps and every third-party image arrive, and
// they are not hidden below the fold for it. The store itself — browsing,
// a listing, the install card — lives in ./store.
const DOORS = ["store", "git", "image"] as const;

export default function NewApp() {
  const [door, setDoor] = useTabParam(DOORS, { key: "from" });
  return (
    <div className="space-y-6">
      <PageHeader
        title="Add app"
        description="Pick an app and Fog handles the rest — storage, updates, backups, and a hostname."
      >
        <Tabs
          variant="underline"
          label="Where the app comes from"
          active={door}
          onChange={setDoor}
          items={[
            { id: "store", label: "App store" },
            { id: "git", label: "Your repository" },
            { id: "image", label: "Image or compose" },
          ]}
        />
      </PageHeader>
      {door === "store" && <Store />}
      {door === "git" && <RepositoryDoor />}
      {door === "image" && (
        <>
          <ImageDoor />
          <ManifestImport />
        </>
      )}
    </div>
  );
}

// ---- the two hand-built doors: a repository, or an image / compose file ----

// Both doors share one shape: the one field that defines the app up top,
// the name beside it, and the rest of the spec folded under More options —
// the defaults are right for most apps, and a wall of forty inputs is the
// thing the store exists to avoid. The full spec editor stays one click
// away, not on another page.
function useCreateApp() {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const { run, busy, error } = useAction();

  // review: the spec came from a pasted compose file — somebody else's —
  // so the app is created held, and its page opens on the review.
  async function create(name: string, draft: SpecDraft, review = false) {
    await run(async () => {
      const { spec, secrets } = draftToPayload(draft);
      await post("/api/apps", { name: name.trim(), spec, secrets, review });
      await qc.invalidateQueries({ queryKey: appsKey });
      navigate(paths.app(name.trim()));
    }, "Create failed", { holdOnSuccess: true });
  }
  return { create, error, busy };
}

function MoreOptions({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="rounded-lg border border-hair">
      <button type="button" onClick={() => setOpen(!open)}
        className="flex w-full items-center justify-between px-4 py-3 text-sm text-ink-muted hover:text-ink">
        <span>{open ? "▾" : "▸"} More options</span>
        <span className="text-xs text-ink-muted">environment, volumes, routes, runtime…</span>
      </button>
      {open && <div className="border-t border-hair p-4">{children}</div>}
    </div>
  );
}

function RepositoryDoor() {
  const [name, setName] = useState("");
  const [draft, setDraft] = useState<SpecDraft>(() => ({ ...emptyDraft(), sourceType: "git" }));
  const { create, error, busy } = useCreateApp();
  const repoError = validateGitRepo(draft.repo);

  return (
    <form
      onSubmit={(e) => { e.preventDefault(); create(name, draft); }}
      className="max-w-3xl space-y-5"
    >
      <p className="text-sm text-ink-muted">
        Fog clones the repository, builds its Dockerfile on this box, and runs the result. On create
        it generates a deploy key for you to add to the repository, and a webhook for push-to-deploy.
      </p>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Name" value={name} onChange={(e) => setName(e.target.value)}
          placeholder="my-app" hint="Lowercase slug — becomes the container name, network alias, and default hostname." required />
        <Field label="Branch" value={draft.branch} onChange={(e) => setDraft({ ...draft, branch: e.target.value })} placeholder="main" />
      </div>
      <div>
        <Field label="Repository (SSH)" value={draft.repo} onChange={(e) => setDraft({ ...draft, repo: e.target.value })}
          placeholder="ssh://git@git.example.com/you/app.git" required />
        {repoError && <p className="mt-1 text-xs text-warn">{repoError}</p>}
      </div>
      <MoreOptions>
        <SpecEditor draft={draft} onChange={setDraft} source={false} app={name} />
      </MoreOptions>
      {error && <p className="text-sm text-warn">{error}</p>}
      <button type="submit" disabled={busy || !name.trim() || !draft.repo.trim() || !!repoError}
        className="rounded-md bg-accent px-4 py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
        {busy ? "Creating…" : "Create app"}
      </button>
      <p className="text-xs text-ink-muted">
        The app is created but not deployed: the next page walks through the deploy key, the webhook, and the first build.
      </p>
    </form>
  );
}

function ImageDoor() {
  const [name, setName] = useState("");
  const [draft, setDraft] = useState<SpecDraft>(() => emptyDraft());
  const { create, error, busy } = useCreateApp();
  const [showImport, setShowImport] = useState(false);
  const [composeText, setComposeText] = useState("");
  const [imported, setImported] = useState<ImportedService[] | null>(null);
  const [warnings, setWarnings] = useState<string[]>([]);
  const [importError, setImportError] = useState("");
  // A spec that came from a pasted compose file is created held for review.
  const [fromCompose, setFromCompose] = useState(false);

  async function importCompose() {
    setImportError("");
    try {
      const res = await post<{ services: ImportedService[] }>("/api/apps/import-compose", { yaml: composeText });
      if (res.services.length === 1) applyImport(res.services[0]);
      else setImported(res.services);
    } catch (err) {
      setImportError(errorText(err, "Import failed"));
    }
  }

  function applyImport(svc: ImportedService) {
    setFromCompose(true);
    setName(svc.name);
    setDraft(draftFromSpec(svc.spec, []));
    setWarnings(svc.warnings ?? []);
    setImported(null);
    setShowImport(false);
  }

  return (
    <form
      onSubmit={(e) => { e.preventDefault(); create(name, draft, fromCompose); }}
      className="max-w-3xl space-y-5"
    >
      <p className="text-sm text-ink-muted">
        Any container image runs here. Have a docker-compose file from the project's docs? Paste it and
        Fog converts the service into an app.
      </p>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Name" value={name} onChange={(e) => setName(e.target.value)}
          placeholder="my-app" hint="Lowercase slug — becomes the container name, network alias, and default hostname." required />
        <Field label="Image" value={draft.image} onChange={(e) => setDraft({ ...draft, image: e.target.value })}
          placeholder="lscr.io/linuxserver/sonarr:latest" required />
      </div>

      <div className="rounded-lg border border-hair">
        <button type="button" onClick={() => setShowImport(!showImport)}
          className="flex w-full items-center justify-between px-4 py-3 text-sm text-ink-muted hover:text-ink">
          <span>{showImport ? "▾" : "▸"} Paste a docker-compose file instead</span>
          <span className="text-xs text-ink-muted">fills in everything above and below</span>
        </button>
        {showImport && (
          <div className="space-y-2 border-t border-hair p-4">
            <textarea value={composeText} onChange={(e) => setComposeText(e.target.value)}
              placeholder="services:&#10;  myapp:&#10;    image: …"
              className={`${controlArea} h-48 w-full font-mono text-xs`} />
            <button type="button" onClick={importCompose} disabled={!composeText.trim()}
              className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
              Convert to app
            </button>
            {importError && <p className="text-sm text-warn">{importError}</p>}
            {imported && (
              <div className="space-y-1 text-sm">
                <p className="text-ink-muted">Multiple services found — pick one to import:</p>
                {imported.map((s) => (
                  <button key={s.name} type="button" onClick={() => applyImport(s)}
                    className="mr-2 rounded-md border border-edge px-2 py-1 text-ink hover:bg-fog-800">
                    {s.name}
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
      {warnings.length > 0 && (
        <div className="rounded-lg border border-warn/30 bg-warn/10 p-3 text-sm text-warn">
          <p className="mb-1 font-medium">Imported with notes:</p>
          <ul className="list-inside list-disc space-y-0.5 text-xs">
            {warnings.map((w, i) => <li key={i}>{w}</li>)}
          </ul>
        </div>
      )}

      <MoreOptions>
        <SpecEditor draft={draft} onChange={setDraft} source={false} app={name} />
      </MoreOptions>
      {error && <p className="text-sm text-warn">{error}</p>}
      <button type="submit" disabled={busy || !name.trim() || !draft.image.trim()}
        className="rounded-md bg-accent px-4 py-2 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
        {busy ? "Creating…" : "Create & deploy"}
      </button>
    </form>
  );
}

// ---- a Fog manifest, exported from this server or another ----

// A manifest is a whole spec somebody else wrote, so importing one creates
// the app held: the next page is its review — every host folder, port,
// grant and acknowledgement it asks for — and nothing deploys until the
// operator confirms it there.
function ManifestImport() {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [text, setText] = useState("");
  const { run, busy, error } = useAction();

  async function readFile(f: File | undefined) {
    if (f) setText(await f.text());
  }

  async function importManifest() {
    await run(async () => {
      const res = await post<AppImportResult>("/api/apps/import", { yaml: text });
      await qc.invalidateQueries({ queryKey: appsKey });
      navigate(paths.app(res.name));
    }, "Import failed", { holdOnSuccess: true });
  }

  return (
    <div className="max-w-3xl rounded-lg border border-hair">
      <button type="button" onClick={() => setOpen(!open)}
        className="flex w-full items-center justify-between px-4 py-3 text-sm text-ink-muted hover:text-ink">
        <span>{open ? "▾" : "▸"} Import a Fog manifest</span>
        <span className="text-xs text-ink-muted">a .fog.yaml exported from a Fog server</span>
      </button>
      {open && (
        <div className="space-y-2 border-t border-hair p-4">
          <p className="text-xs text-ink-muted">
            The app is created but not deployed. You'll see everything it mounts, publishes and asks for, and
            it runs only once you confirm. Secrets aren't in a manifest — you'll enter them on the app's page.
          </p>
          <input type="file" accept=".yaml,.yml" onChange={(e) => readFile(e.target.files?.[0])}
            className="text-xs text-ink-muted" />
          <textarea value={text} onChange={(e) => setText(e.target.value)}
            placeholder="name: myapp&#10;spec:&#10;  source: …"
            className={`${controlArea} h-48 w-full font-mono text-xs`} />
          {error && <p className="text-sm text-warn">{error}</p>}
          <button type="button" onClick={importManifest} disabled={busy || !text.trim()}
            className="rounded-md bg-accent px-3 py-1.5 text-sm font-semibold text-accent-on hover:bg-accent-hover disabled:opacity-50">
            {busy ? "Importing…" : "Import for review"}
          </button>
        </div>
      )}
    </div>
  );
}
