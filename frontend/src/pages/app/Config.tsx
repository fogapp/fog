// The config tab reads as a document: what this app is actually set to, laid
// out to be read, with an Edit beside each group that swaps that group — and
// only that group — for its form. Nothing is a text box until you ask
// it to be, so the page stops presenting forty inputs to an eye that came
// looking for one value. Changes collect in a draft and announce themselves
// in a bar pinned to the bottom of the window; nothing reaches the server
// until you say so there.

import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";
import { post, put, type AppDetail as AppDetailT } from "../../api";
import Button from "../../components/Button";
import GitAccess from "../../components/GitAccess";
import { SectionHead } from "../../components/Section";
import Markdown from "../../components/Markdown";
import { routeHost } from "../../lib/hostname";
import { useFlash } from "../../lib/useFlash";
import { paths } from "../../lib/paths";
import { filesCrumbs } from "../../lib/volumePlacement";
import {
  AccessEditor, DeployEditor, EnvEditor, FilesEditor, NotesEditor, PortsEditor, PostgresEditor,
  RoutesEditor, RuntimeEditor, SecretsEditor, SourceEditor, VolumesEditor,
  draftFromSpec, draftToPayload, type SpecDraft,
} from "../../components/SpecEditor";
import { settingsQuery } from "../../queries";
import { useAction } from "../../lib/useAction";
import { fleetSnapshotsBeforeUpdate } from "../../lib/backupPolicy";
import { control } from "../../components/Field";

const POLICY: Record<SpecDraft["updatePolicy"], string> = {
  manual: "Manual — you update when you choose",
  notify: "Notify — tell me when a new image ships",
  auto: "Auto — snapshot, apply, verify",
};

export function Config({ name, detail, onSaved }: { name: string; detail: AppDetailT; onSaved: () => Promise<unknown> }) {
  // draft and baseline are one piece of state so they can never be updated
  // half-way: the baseline moves only when a save actually succeeds.
  const seed = () => {
    const d = draftFromSpec(detail.spec, detail.secretKeys);
    return { draft: d, baseline: d, rev: detail.latestRev };
  };
  const [{ draft, baseline, rev }, setState] = useState(seed);
  // The spec can move under an open tab — a webhook deploy, a rollback, our
  // own save landing. When the server's latest revision changes, re-seed from
  // it during render so the page never presents an old revision as current.
  // Unsaved edits were made against the revision that just went away; they go
  // with it rather than silently overwriting the newer spec on the next save.
  if (rev !== detail.latestRev) setState(seed());
  // ?edit=volumes opens straight onto a group's editor: the Drive tab's
  // "Move…" and the overview's "Needs you" land here with the form open.
  const [params, setParams] = useSearchParams();
  const [open, setOpen] = useState<string | null>(params.get("edit"));
  useEffect(() => {
    const e = params.get("edit");
    if (e) {
      setOpen(e);
      const next = new URLSearchParams(params);
      next.delete("edit");
      setParams(next, { replace: true });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [params]);
  const [comment, setComment] = useState("");
  const { run, busy, error, setError } = useAction();
  const [saved, flashSaved] = useFlash();
  const settings = useQuery(settingsQuery);
  const domain = settings.data?.base_domain;

  const set = (patch: Partial<SpecDraft>) => setState((s) => ({ ...s, draft: { ...s.draft, ...patch } }));
  // Dirty is judged on what would actually be sent, not on the draft itself:
  // opening an empty group seeds a blank row, and a blank row is not a change.
  const dirty = JSON.stringify(draftToPayload(draft)) !== JSON.stringify(draftToPayload(baseline));

  async function save(deploy: boolean) {
    flashSaved("");
    await run(async () => {
      const { spec, secrets } = draftToPayload(draft);
      await put(`/api/apps/${name}`, { spec, secrets, comment, deploy });
      await onSaved();
      // Only now is the draft the truth about this app, so only now does it
      // become the thing later edits are measured against.
      setState((s) => ({ ...s, baseline: s.draft }));
      setComment("");
      setOpen(null);
      flashSaved(deploy ? "Saved — deploy started, see Overview for progress" : "Saved");
    }, "Save failed");
  }

  // Closing an editor drops the rows it left completely blank — a half-opened
  // "add a variable" shouldn't leave an "unnamed / empty" line in a document
  // whose whole job is being readable.
  const closeEditor = () => {
    setState((s) => ({ ...s, draft: tidy(s.draft) }));
    setOpen(null);
  };

  const cancel = () => {
    setState((s) => ({ ...s, draft: s.baseline }));
    setOpen(null);
    setError("");
  };

  const props = { draft, set };
  const isGit = draft.sourceType === "git";
  const access = accessOf(draft.uses);

  // available: can this app have the group at all. present: does it have one.
  // A group that's neither is offered as a link at the foot of the document
  // rather than as an empty heading nobody asked for.
  const sections: {
    id: string; title: string; available: boolean; present: boolean;
    doc: React.ReactNode; editor?: React.ReactNode; seed?: () => void;
  }[] = [
    {
      id: "source", title: "Source", available: true, present: true,
      editor: <SourceEditor {...props} />,
      doc: isGit ? (
        <>
          <Row label="Repository">{val(draft.repo)}</Row>
          <Row label="Branch">{draft.branch ? <Mono>{draft.branch}</Mono> : <Dim>main (default)</Dim>}</Row>
          <Row label="Dockerfile">{draft.dockerfile ? <Mono>{draft.dockerfile}</Mono> : <Dim>Dockerfile (default)</Dim>}</Row>
          {draft.buildArgs.filter((a) => a.k.trim()).map((a) => (
            <Row key={a.k} label={a.k} mono>{val(a.v)}</Row>
          ))}
          {/* The credentials the repository needs from fog live with the
              source they unlock, not on a tab of their own. */}
          {detail.git && <GitAccess name={name} git={detail.git} />}
        </>
      ) : (
        <Row label="Image">{val(draft.image)}</Row>
      ),
    },
    {
      id: "deploy", title: "Deploy", available: isGit,
      present: !!draft.release.trim() || draft.processes.length > 0,
      editor: <DeployEditor {...props} />,
      doc: (
        <>
          <Row label="Release command">{val(draft.release)}</Row>
          {draft.processes.map((p, i) => (
            <Row key={i} label={p.name || "unnamed"} mono>{val(p.command)}</Row>
          ))}
        </>
      ),
    },
    {
      id: "postgres", title: "Managed Postgres", available: isGit, present: !!draft.pgDatabases.trim(),
      editor: <PostgresEditor {...props} />,
      doc: (
        <>
          <Row label="Databases">{val(draft.pgDatabases)}</Row>
          <Row label="Extensions">{val(draft.pgExtensions)}</Row>
          {draft.pgInject.filter((e) => e.k.trim()).map((e) => (
            <Row key={e.k} label={e.k} mono>{val(e.v)}</Row>
          ))}
        </>
      ),
    },
    {
      id: "env", title: "Environment", available: true, present: draft.env.length > 0,
      editor: <EnvEditor {...props} />,
      seed: () => set({ env: [...draft.env, { k: "", v: "" }] }),
      doc: draft.env.map((e, i) => (
        <Row key={i} label={e.k || "unnamed"} mono>{e.v ? <Mono>{e.v}</Mono> : <Dim>empty</Dim>}</Row>
      )),
    },
    {
      id: "secrets", title: "Secrets", available: true, present: draft.secrets.length > 0,
      editor: <SecretsEditor {...props} />,
      seed: () => set({ secrets: [...draft.secrets, { k: "", v: "", existing: false }] }),
      // The value is never known here — fogd only ever sends the names back —
      // so the document says it is stored, and says separately when a new one
      // is sitting in the draft waiting to be saved.
      doc: draft.secrets.map((s, i) => (
        <Row key={i} label={s.k || "unnamed"} mono>
          {s.v !== "" ? (
            <span className="text-accent">new value — not saved yet</span>
          ) : s.existing ? (
            <Dim>•••••• stored</Dim>
          ) : (
            <Dim>no value yet</Dim>
          )}
        </Row>
      )),
    },
    {
      id: "volumes", title: "Volumes", available: true, present: draft.volumes.length > 0,
      editor: <VolumesEditor {...props} app={name} />,
      seed: () => set({ volumes: [...draft.volumes, { name: "", mount: "", files: "", host: "", owner: "", readOnly: false }] }),
      doc: draft.volumes.map((v, i) => (
        <Row key={i} label={v.name || "unnamed"} mono>
          <Mono>{v.mount || "—"}</Mono>{v.readOnly && <span className="ml-1 text-xs text-ink-muted">read-only</span>}
          <Dim>
            {" · "}
            {v.files ? (
              <Link to={paths.files(v.files)} className="text-ink-muted hover:text-ink hover:underline" title="Open this folder in Drive">
                {filesCrumbs(v.files).join(" › ")}
              </Link>
            ) : v.host ? `host ${v.host}` : "Fog-managed, backed up"}
            {v.owner ? ` · owner ${v.owner}` : ""}
          </Dim>
        </Row>
      )),
    },
    {
      id: "aux", title: "Sidecar services", available: draft.aux.length > 0, present: true,
      doc: draft.aux.map((a) => (
        <Row key={a.name} label={a.name} mono><Mono>{a.image}</Mono></Row>
      )),
    },
    {
      id: "routes", title: "Routes", available: true, present: draft.routes.length > 0,
      editor: <RoutesEditor {...props} />,
      seed: () => set({ routes: [...draft.routes, { host: "", port: "", noBuffer: false, auth: false, lanOnly: false }] }),
      doc: draft.routes.map((r, i) => {
        const host = routeHost(name, r.host || undefined, domain);
        return (
          <Row key={i} label={host || r.host || name} mono>
            <Mono>→ :{r.port || "?"}</Mono>
            {(r.noBuffer || r.auth) && (
              <Dim>{` · ${[r.noBuffer && "no buffering", r.auth && "fog login"].filter(Boolean).join(" · ")}`}</Dim>
            )}
            {!host && <Dim> · hostname needs a base domain</Dim>}
          </Row>
        );
      }),
    },
    {
      id: "ports", title: "Published ports", available: true, present: draft.ports.length > 0,
      editor: <PortsEditor {...props} />,
      seed: () => set({ ports: [...draft.ports, { port: "", hostPort: "", protocol: "tcp" }] }),
      doc: draft.ports.map((p, i) => (
        <Row key={i} label={`${p.port || "?"}/${p.protocol}`} mono>
          <Mono>host {p.hostPort || p.port || "?"}</Mono>
        </Row>
      )),
    },
    {
      id: "runtime", title: "Runtime", available: true, present: true,
      editor: <RuntimeEditor {...props} />,
      doc: (
        <>
          <Row label="Health check"><HealthCheck name={name} path={draft.healthPath} /></Row>
          <Row label="Memory limit">{val(draft.memoryLimit)}</Row>
          {!isGit && <Row label="Update policy">{POLICY[draft.updatePolicy]}</Row>}
          {/* The schedule is edited on the Drive tab — one editor, so the
              two can never disagree about what is saved. */}
          <Row label="Backups">
            {draft.backupEnabled ? (
              <>
                Included in nightly backups
                <Dim>
                  {draft.backupHourUTC.trim()
                    ? ` · at ${draft.backupHourUTC.trim().padStart(2, "0")}:00 UTC`
                    : " · at the global default hour"}
                  {(draft.backupBeforeUpdate ?? fleetSnapshotsBeforeUpdate(settings.data)) ? " · snapshot before every update" : ""}
                </Dim>
              </>
            ) : (
              <Dim>not backed up</Dim>
            )}
            <Dim>{" · "}</Dim>
            <Link to={paths.app(name, "drive", { tab: "schedule" })} className="text-xs text-ink-muted underline decoration-dotted underline-offset-2 hover:text-ink">
              edit on the Drive tab
            </Link>
          </Row>
        </>
      ),
    },
    {
      // What this app may read from Fog itself (spec.uses). The apps it
      // is handed are in the same list; they are connected on the
      // Integrations tab.
      id: "access", title: "App access", available: true,
      present: !!draft.uses.trim() || !!draft.connection,
      editor: <AccessEditor {...props} />,
      doc: (
        <>
          <Row label="Reads">
            {access.reads.length ? access.reads.join(" and ") : <Dim>nothing beyond its own</Dim>}
          </Row>
          {access.posts && <Row label="Posts">into chat, in the rooms fog's bot is in, as itself</Row>}
          {access.apps.length > 0 && (
            <Row label="Connected to">
              {access.apps.join(", ")}
              <Dim>{" · "}</Dim>
              <Link to={paths.app(name, "integrations")} className="text-xs text-ink-muted underline decoration-dotted underline-offset-2 hover:text-ink">
                manage on the Integrations tab
              </Link>
            </Row>
          )}
          {draft.connection && (
            <Row label="Offers">
              port <Mono>{draft.connection.port}{draft.connection.path ?? ""}</Mono>
              {draft.connection.shares?.length ? <Dim>{` · shares ${draft.connection.shares.join(", ")}`}</Dim> : null}
            </Row>
          )}
        </>
      ),
    },
    {
      id: "notes", title: "Notes", available: true, present: !!draft.notes.trim(),
      editor: <NotesEditor {...props} />,
      doc: <Markdown>{draft.notes}</Markdown>,
    },
    {
      id: "files", title: "Config files", available: true, present: draft.files.length > 0,
      editor: <FilesEditor {...props} />,
      seed: () => set({ files: [...draft.files, { name: "", mount: "", content: "" }] }),
      doc: draft.files.map((f, i) => (
        <Row key={i} label={f.name || "unnamed"} mono>
          <Mono>{f.mount || "—"}</Mono>
          {f.content && (
            <details className="mt-1">
              <summary className="cursor-pointer text-xs text-ink-muted hover:text-ink">contents</summary>
              <pre className="mt-1 max-h-64 overflow-auto rounded border border-hair bg-code p-2 text-xs text-ink-muted">
                {f.content}
              </pre>
            </details>
          )}
        </Row>
      )),
    },
  ];

  const shown = sections.filter((s) => s.available && (s.present || open === s.id));
  const addable = sections.filter((s) => s.available && !s.present && open !== s.id && s.editor);

  return (
    <div className="min-w-0">
      {saved && <p className="pb-2 text-sm font-medium text-accent">✓ {saved}</p>}

      {shown.map((s, i) => (
        <section key={s.id} id={s.id} className={i === 0 ? "" : "mt-9"}>
          <SectionHead
            title={s.title}
            right={s.editor && (
              <Button size="sm" variant={open === s.id ? "secondary" : "ghost"} onClick={() => (open === s.id ? closeEditor() : setOpen(s.id))}>
                {open === s.id ? "Done" : "Edit"}
              </Button>
            )}
          />
          <div className="mt-3">
            {open === s.id ? (
              <div className="rounded-lg border border-veil-edge bg-veil p-4">{s.editor}</div>
            ) : (
              <dl>{s.doc}</dl>
            )}
          </div>
        </section>
      ))}

      {addable.length > 0 && (
        <div className="mt-9 flex flex-wrap items-baseline gap-x-3 gap-y-1 border-t border-divider pt-5 text-xs text-ink-muted">
          <span>Add</span>
          {addable.map((s) => (
            <button
              key={s.id}
              onClick={() => { s.seed?.(); setOpen(s.id); }}
              className="underline decoration-dotted underline-offset-2 hover:text-ink"
            >
              {s.title.toLowerCase()}
            </button>
          ))}
        </div>
      )}

      {/* The bar exists only when there is something to save, and it says what
          it is saving — a config page that always shows Save invites clicking
          it to find out whether anything happened. */}
      {dirty && (
        <div className="sticky bottom-0 z-20 -mx-4 flex flex-wrap items-center gap-3 border-t border-hair bg-fog-950/95 px-4 py-3 backdrop-blur sm:-mx-6 sm:px-6 lg:-mx-8 lg:px-8">
          <span className="text-sm font-medium text-ink">Unsaved changes</span>
          <input
            className={`${control} min-w-0 flex-1`}
            placeholder="Change comment (optional)"
            value={comment}
            onChange={(e) => setComment(e.target.value)}
          />
          {error && <p className="w-full text-sm text-warn sm:w-auto">{error}</p>}
          <Button onClick={cancel} disabled={busy}>Cancel</Button>
          <Button onClick={() => save(false)} disabled={busy}>{busy ? "Saving…" : "Save only"}</Button>
          <Button variant="primary" onClick={() => save(true)} disabled={busy}>
            {busy ? "Saving…" : "Save & deploy"}
          </Button>
        </div>
      )}
    </div>
  );
}

// One line of the document. Labels are the app's own words — an env var name,
// a volume name — so they set in mono; the fixed ones read as prose.
function Row({ label, mono, children }: { label: string; mono?: boolean; children: React.ReactNode }) {
  return (
    <div className="grid gap-x-8 gap-y-0.5 py-1 sm:grid-cols-[minmax(0,16rem)_minmax(0,1fr)]">
      <dt className={`min-w-0 break-all sm:pt-px ${mono ? "font-mono text-xs text-ink-muted" : "text-cell text-ink-muted"}`}>
        {label}
      </dt>
      <dd className="min-w-0 text-sm text-ink">{children}</dd>
    </div>
  );
}

const Mono = ({ children }: { children: React.ReactNode }) => (
  <span className="break-all font-mono text-[13px] text-ink">{children}</span>
);

const Dim = ({ children }: { children: React.ReactNode }) => <span className="text-ink-muted">{children}</span>;

interface HealthCheckResult {
  ok: boolean; url?: string; status?: number; statusText?: string; contentType?: string;
  body?: string; truncated?: boolean; durationMs: number; error?: string;
}

// The health path with a Check beside it: one probe the way the monitor makes
// it, answered in full — status, timing, and what the app actually said.
function HealthCheck({ name, path }: { name: string; path: string }) {
  const check = useMutation({
    mutationFn: () => post<HealthCheckResult>(`/api/apps/${name}/health-check`, { path: path.trim() }),
  });
  // A result belongs to the path it was run against; editing the path clears it.
  const [ranFor, setRanFor] = useState("");
  const r = check.data && ranFor === path.trim() && !check.isPending ? check.data : undefined;
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-3">
        {val(path)}
        {path.trim() && (
          <Button size="sm" disabled={check.isPending}
            onClick={() => { setRanFor(path.trim()); check.mutate(); }}>
            {check.isPending ? "Checking…" : "Check"}
          </Button>
        )}
      </div>
      {check.isError && ranFor === path.trim() && !check.isPending && (
        <p className="text-xs text-warn">{(check.error as Error).message}</p>
      )}
      {r && (
        <div className="space-y-1 rounded-md border border-hair p-2 text-xs">
          <p>
            <span className={r.ok ? "text-accent" : "text-warn"}>
              {r.status ? `${r.status} ${r.statusText ?? ""}` : "No response"}
            </span>
            <Dim>{` · ${r.ok ? "healthy" : "unhealthy"}${r.status || r.durationMs ? ` · ${r.durationMs} ms` : ""}`}</Dim>
          </p>
          {r.url && <p className="break-all font-mono text-ink-muted">GET {r.url}</p>}
          {r.error && !r.status && <p className="text-warn">{r.error}</p>}
          {r.body !== undefined && r.body !== "" && (
            <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded bg-veil p-2 font-mono text-ink-muted">
              {r.body}{r.truncated ? "\n…" : ""}
            </pre>
          )}
          {r.status !== undefined && r.status > 0 && !r.body && <Dim>Empty body</Dim>}
        </div>
      )}
    </div>
  );
}

const val = (s: string) => (s.trim() ? <Mono>{s}</Mono> : <Dim>not set</Dim>);

const blank = (...xs: string[]) => xs.every((x) => !x.trim());

function tidy(d: SpecDraft): SpecDraft {
  return {
    ...d,
    buildArgs: d.buildArgs.filter((e) => !blank(e.k, e.v)),
    env: d.env.filter((e) => !blank(e.k, e.v)),
    secrets: d.secrets.filter((e) => e.existing || !blank(e.k, e.v)),
    volumes: d.volumes.filter((v) => !blank(v.name, v.mount, v.files, v.host, v.owner)),
    files: d.files.filter((f) => !blank(f.name, f.mount, f.content)),
    routes: d.routes.filter((r) => !blank(r.host, r.port)),
    ports: d.ports.filter((p) => !blank(p.port, p.hostPort)),
    processes: d.processes.filter((p) => !blank(p.name, p.command)),
    pgInject: d.pgInject.filter((e) => !blank(e.k, e.v)),
  };
}

// accessOf splits a draft's uses into what it reads from Fog, what it may
// do through Fog, and the apps it is connected to.
function accessOf(uses: string): { reads: string[]; posts: boolean; apps: string[] } {
  const list = uses.split(",").map((u) => u.trim()).filter(Boolean);
  const reads = [list.includes("events") && "the event stream", list.includes("home") && "the home view"]
    .filter((r): r is string => !!r);
  return { reads, posts: list.includes("chat"), apps: list.filter((u) => u !== "events" && u !== "home" && u !== "chat") };
}
