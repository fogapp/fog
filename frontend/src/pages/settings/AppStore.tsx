// Settings › Preferences › App store: which catalog entries appear when adding an app.
// The whole catalog as one table — the icon and the name, what kind of
// thing it is, whether this machine can run it, whether it is already
// here — and a switch per row. The switch is the only control; the name
// opens the listing for anyone who wants to read before deciding.

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient, useMutation } from "@tanstack/react-query";
import { put } from "../../api";
import AppIcon from "../../components/AppIcon";
import { LoadError, Loading, errorText } from "../../components/Async";
import Field, { FilterInput, TextArea } from "../../components/Field";
import Toggle from "../../components/Toggle";
import { useApps } from "../../lib/useApps";
import { approx } from "../../lib/useCapacity";
import { useFlash } from "../../lib/useFlash";
import { paths } from "../../lib/paths";
import { SettingsPage } from "./Frame";
import { useSettingsForm } from "./useSettingsForm";
import { catalogQuery, settingsQuery } from "../../queries";

export default function AppStore() {
  const qc = useQueryClient();
  const catalog = useQuery({
    ...catalogQuery,
  });
  const apps = useApps();
  const installed = new Set((apps.data ?? []).map((a) => a.name));
  const [saved, flashSaved] = useFlash(1500);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState("");

  const enabled = new Set(catalog.data?.enabled ?? []);

  const save = useMutation({
    mutationFn: (ids: string[]) => put("/api/settings", { catalog_enabled: JSON.stringify(ids) }),
    onSuccess: async () => {
      // Awaited: the switches only settle once the server's list is back,
      // so a rejected toggle never leaves one flipped.
      await Promise.all([
        qc.invalidateQueries({ queryKey: catalogQuery.queryKey }),
        qc.invalidateQueries({ queryKey: settingsQuery.queryKey }),
      ]);
      setError("");
      flashSaved("Saved ✓");
    },
    onError: (e) => setError(errorText(e, "Save failed")),
  });

  function toggle(id: string, on: boolean) {
    const next = new Set(enabled);
    if (on) next.add(id);
    else next.delete(id);
    save.mutate([...next]);
  }

  const entries = catalog.data?.entries ?? [];
  const q = filter.trim().toLowerCase();
  const rows = entries.filter(
    (e) => !q || e.title.toLowerCase().includes(q) || e.category.toLowerCase().includes(q) || e.description.toLowerCase().includes(q),
  );

  return (
    <SettingsPage description="The apps offered under Add app. Switching one off hides it from the store; anything already installed keeps running.">
      <div className="space-y-4">
        <AppSources unsourced={catalog.data?.unsourced ?? 0} />
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <FilterInput value={filter} onChange={setFilter} label="Filter the catalog" />
          <span className="flex-1" />
          {catalog.data && (
            <span className="text-hint tabular-nums text-ink-muted">
              {enabled.size} of {entries.length} in the store
            </span>
          )}
          <span className="h-5 text-sm text-accent">{saved}</span>
        </div>
        {error && <p className="text-sm text-warn">{error}</p>}
        {/* The columns answer to the table's own width, not the window's:
            inside the Settings frame a wide window is still a narrow table,
            and the switch column must never be the one scrolled away. */}
        {/* Unflipped switches are a statement about what's disabled — don't
            draw the table at all until the real selection is in hand. */}
        {!catalog.data && !catalog.isError && <Loading label="Loading the app catalog…" />}
        {!catalog.data && catalog.isError && (
          <LoadError label="Couldn't load the app catalog." error={catalog.error} onRetry={() => catalog.refetch()} />
        )}
        {catalog.data && (
          <div className="@container overflow-x-auto rounded-lg border border-veil-edge bg-veil">
            <table className="w-full text-left text-cell">
              <thead>
                <tr className="caps-label border-b border-hair-strong">
                  <th className="px-4 py-2.5 font-medium">App</th>
                  <th className="hidden px-3 py-2.5 font-medium @md:table-cell">Category</th>
                  <th className="hidden px-3 py-2.5 font-medium @5xl:table-cell">Source</th>
                  <th className="hidden px-3 py-2.5 font-medium @3xl:table-cell">Memory</th>
                  <th className="hidden px-3 py-2.5 font-medium @2xl:table-cell">Status</th>
                  <th className="whitespace-nowrap px-4 py-2.5 text-right font-medium">In store</th>
                </tr>
              </thead>
              <tbody>
                {rows.length === 0 && (
                  <tr>
                    <td colSpan={6} className="px-4 py-6 text-center text-sm text-ink-muted">
                      Nothing matches “{filter.trim()}”.
                    </td>
                  </tr>
                )}
                {rows.map((e) => (
                  <tr key={e.id} className="border-b border-divider last:border-0">
                    {/* w-full + max-w-0: the name column takes what the others
                        leave, and its description truncates inside that. */}
                    <td className="w-full max-w-0 px-4 py-2.5">
                      <div className="flex items-center gap-3">
                        <AppIcon entry={e} size={32} />
                        <div className="min-w-0">
                          <Link to={paths.storeApp(e.id)} className="font-medium text-ink hover:underline">{e.title}</Link>
                          <p className="hidden truncate text-xs text-ink-muted sm:block">{e.description}</p>
                        </div>
                      </div>
                    </td>
                    <td className="hidden px-3 py-2.5 text-ink-muted @md:table-cell">{e.category}</td>
                    <td className="hidden px-3 py-2.5 @5xl:table-cell">
                      {e.spec.source.type === "git" ? (
                        <span className="text-xs text-ink-muted">Git repository</span>
                      ) : (
                        <span className="font-mono text-xs text-ink-muted" title={e.spec.source.image}>
                          {(e.spec.source.image ?? "").replace(/^(docker\.io|lscr\.io|ghcr\.io)\//, "").replace(/:[^:]+$/, "")}
                        </span>
                      )}
                    </td>
                    <td className="hidden whitespace-nowrap px-3 py-2.5 @3xl:table-cell">
                      {e.footprint ? (
                        <>
                          <span className="tabular-nums text-ink-muted">~{approx(e.footprint.memory)}</span>
                          <span className="ml-2 text-xs text-ink-muted" title={`${e.footprint.cpu} on the processor`}>{e.footprint.cpu}</span>
                        </>
                      ) : (
                        <span className="text-ink-muted">—</span>
                      )}
                    </td>
                    <td className="hidden px-3 py-2.5 text-xs @2xl:table-cell">
                      {installed.has(e.id) ? (
                        <Link to={paths.app(e.id)} className="rounded-full bg-accent/10 px-2 py-0.5 font-medium text-accent hover:underline">
                          installed
                        </Link>
                      ) : e.blocker ? (
                        <span className="rounded-full bg-warn/10 px-2 py-0.5 font-medium text-warn" title={e.blocker}>
                          can't run here
                        </span>
                      ) : (
                        <span className="text-ink-muted">—</span>
                      )}
                    </td>
                    <td className="px-4 py-2.5 text-right">
                      <Toggle
                        checked={enabled.has(e.id)}
                        onChange={(on) => toggle(e.id, on)}
                        disabled={save.isPending}
                        aria-label={`Show ${e.title} in the store`}
                      />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </SettingsPage>
  );
}

// Overrides are stored as a JSON object; here they read one per line,
// "name = clone URL", which is what a person would write.
function overridesToText(json: string): string {
  if (!json) return "";
  try {
    return Object.entries(JSON.parse(json) as Record<string, string>).map(([k, v]) => `${k} = ${v}`).join("\n");
  } catch {
    return json;
  }
}

function textToOverrides(text: string): string {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i < 1) return t; // the server refuses it, and says why
    out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return Object.keys(out).length ? JSON.stringify(out) : "";
}

// Where the household's own apps (Fin, Hub, Newtab…) are cloned from.
// A published one carries its own address; the rest need a base here, and
// until one is set they stay out of the store.
function AppSources({ unsourced }: { unsourced: number }) {
  const { value, setValue, SaveButton, loading } = useSettingsForm(["catalog_source_base", "catalog_source_overrides"]);
  const [overrides, setOverrides] = useState<string | null>(null);
  // A saved source changes which apps the store offers: re-read it.
  const qc = useQueryClient();
  const stored = useQuery(settingsQuery).data;
  const sourceKey = `${stored?.catalog_source_base ?? ""}\n${stored?.catalog_source_overrides ?? ""}`;
  useEffect(() => {
    if (stored) qc.invalidateQueries({ queryKey: catalogQuery.queryKey });
  }, [sourceKey]); // eslint-disable-line react-hooks/exhaustive-deps
  if (loading) return <Loading label="Loading app sources…" />;
  const text = overrides ?? overridesToText(value("catalog_source_overrides"));
  return (
    <section className="space-y-3 rounded-lg border border-veil-edge bg-veil p-4">
      <h3 className="font-medium text-ink">First-party app source</h3>
      {unsourced > 0 && (
        <p className="text-sm text-ink-muted">
          {unsourced} household app{unsourced === 1 ? " is" : "s are"} hidden from the store until fog knows where to clone {unsourced === 1 ? "it" : "them"} from.
        </p>
      )}
      <Field label="Repository base" value={value("catalog_source_base")}
        onChange={(e) => setValue("catalog_source_base", e.target.value)}
        placeholder="ssh://git@git.example.com/you/"
        hint="An SSH clone URL ending in /. Each app clones from this plus its name, e.g. …/you/fin.git. Apps already installed keep the address they were installed from." />
      <TextArea label="Per-app overrides" rows={3} value={text}
        onChange={(e) => {
          setOverrides(e.target.value);
          setValue("catalog_source_overrides", textToOverrides(e.target.value));
        }}
        placeholder="fit = ssh://git@github.com/you/fit.git"
        hint="One per line: a repository name, then its clone URL. An override beats both the base and an app's published address." />
      {SaveButton}
    </section>
  );
}
