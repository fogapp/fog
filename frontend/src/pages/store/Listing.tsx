// One app's page in the store: what it is, what it looks like, who makes
// it, and what it needs from this machine — with the install card beside
// it. Laid out the way a store listing is, because that is the page a
// person compares apps on: the pitch and the pictures on the left, the
// facts a scannable column, and the install card top right from the first
// glance — it is the store's one button, with the questions it has to ask.

import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Check } from "lucide-react";
import type { CatalogEntry } from "../../api";
import AppIcon from "../../components/AppIcon";
import CapacityBar from "../../components/CapacityBar";
import { SectionHead } from "../../components/Section";
import { paths } from "../../lib/paths";
import { TONE_TEXT, fitLines, useCapacity, verdict } from "../../lib/useCapacity";
import InstallPanel from "./InstallPanel";

export default function Listing({
  entry,
  all,
  installed,
  onBack,
}: {
  entry: CatalogEntry;
  all: CatalogEntry[];
  // Names of the apps already on this machine; the listing says so
  // instead of offering a second copy.
  installed: Set<string>;
  onBack: () => void;
}) {
  const isInstalled = installed.has(entry.id);
  const shots = entry.screenshots ?? [];
  const paragraphs = (entry.about ?? "").split(/\n\s*\n/).filter(Boolean);
  const links = [
    entry.website && { label: "Website", href: entry.website },
    entry.docs && { label: "Documentation", href: entry.docs },
    entry.projectUrl && { label: "Source code", href: entry.projectUrl },
  ].filter((l): l is { label: string; href: string } => !!l);

  return (
    <div className="space-y-8">
      <button onClick={onBack} className="text-sm text-ink-muted hover:text-ink">← App store</button>

      {/* Two columns from the top: the app on the left, the install card on
          the right where a store keeps its one button — it is the button,
          with the questions it has to ask. On a narrow screen the card
          follows the header, before the pictures. */}
      <div className="grid gap-x-10 gap-y-8 lg:grid-cols-[minmax(0,1fr)_380px] lg:grid-rows-[auto_1fr]">
        <header className="flex flex-wrap items-start gap-5 lg:col-start-1 lg:row-start-1">
          <AppIcon entry={entry} size={96} />
          <div className="min-w-0 flex-1 space-y-1.5">
            <h1 className="text-2xl font-bold tracking-[-.015em] text-ink">{entry.title}</h1>
            <p className="max-w-2xl text-base text-ink-muted">{entry.description}</p>
            <p className="text-hint text-ink-muted">
              {[entry.developer, entry.category, entry.license].filter(Boolean).join(" · ")}
            </p>
            {(isInstalled || entry.blocker) && (
              <div className="flex items-center gap-2 pt-1">
                {isInstalled ? (
                  <>
                    <span className="rounded-full border border-veil-edge bg-hover px-2.5 py-1 text-xs font-medium text-ink-muted">
                      Installed
                    </span>
                    <Link to={paths.app(entry.id)} className="text-sm text-accent hover:underline">Open {entry.title}</Link>
                  </>
                ) : (
                  <span className="rounded-full border border-warn/30 bg-warn/10 px-2.5 py-1 text-xs font-medium text-warn">
                    Can't run here
                  </span>
                )}
              </div>
            )}
          </div>
        </header>

        <aside className="min-w-0 space-y-5 self-start lg:col-start-2 lg:row-start-1 lg:row-span-2">
          <InstallPanel entry={entry} all={all} />
          <MachineFit entry={entry} installed={isInstalled} />
        </aside>

        <div className="min-w-0 space-y-10 lg:col-start-1 lg:row-start-2">
          {entry.blocker && (
            <div className="space-y-1 rounded-lg border border-warn/30 bg-warn/10 p-4">
              <p className="text-sm font-semibold text-warn">This machine can't run {entry.title} yet</p>
              <p className="text-xs leading-relaxed text-warn">{entry.blocker}</p>
            </div>
          )}

          {shots.length > 0 && <Screenshots shots={shots} />}

          <section>
            <SectionHead title="About" />
            <div className="mt-4 max-w-2xl space-y-3">
              {paragraphs.map((p, i) => (
                <p key={i} className="text-[15px] leading-relaxed text-ink">{p}</p>
              ))}
            </div>
          </section>

          {(entry.highlights?.length ?? 0) > 0 && (
            <section>
              <SectionHead title="Highlights" />
              <ul className="mt-4 grid gap-x-8 gap-y-2.5 sm:grid-cols-2">
                {entry.highlights!.map((h) => (
                  <li key={h} className="flex items-start gap-3 text-sm text-ink">
                    <Check aria-hidden className="mt-0.5 size-4 shrink-0 text-ink-muted" />
                    <span>{h}</span>
                  </li>
                ))}
              </ul>
            </section>
          )}

          <section>
            <SectionHead title="Information" />
            <dl className="mt-2 grid gap-x-10 sm:grid-cols-2">
              {facts(entry).map((f) => (
                <div key={f.label} className="flex items-baseline justify-between gap-4 border-b border-divider py-2.5">
                  <dt className="shrink-0 text-hint text-ink-muted">{f.label}</dt>
                  <dd className="min-w-0 text-right text-cell text-ink">{f.value}</dd>
                </div>
              ))}
              {links.map((l) => (
                <div key={l.label} className="flex items-baseline justify-between gap-4 border-b border-divider py-2.5">
                  <dt className="shrink-0 text-hint text-ink-muted">{l.label}</dt>
                  <dd className="min-w-0 truncate text-right text-cell">
                    <a href={l.href} target="_blank" rel="noreferrer" className="text-accent hover:underline">
                      {l.href.replace(/^https?:\/\//, "").replace(/\/$/, "")}
                    </a>
                  </dd>
                </div>
              ))}
            </dl>
          </section>
        </div>
      </div>
    </div>
  );
}

// What installing this would do to the machine: the memory bar with the
// app's own band on it, and a line each for memory, processor and disk,
// with the server's verdict on each. Sits above the install card because
// it is the last thing to read before pressing the button.
// Once the app is installed its cost is already in the machine's figures,
// so a verdict on adding it again would only mislead: the section keeps
// what the app costs and drops the judgement.
function MachineFit({ entry, installed }: { entry: CatalogEntry; installed: boolean }) {
  const cap = useCapacity();
  const fit = installed ? undefined : cap.data?.fits[entry.id];
  const lines = fitLines(entry, fit, cap.data);
  if (lines.length === 0) return null;
  const host = cap.data?.host;
  const apps = cap.data?.apps ?? [];
  const appsMem = apps.reduce((n, a) => n + a.memUsage, 0);
  const other = host ? Math.max(0, host.memUsed - appsMem) : 0;
  const measured = !!fit && !!host && host.memTotal > 0;
  const tone = fit?.tone ?? "ok";
  const dot = (t: string) => (t === "ok" ? "bg-accent" : t === "tight" ? "bg-warn" : "bg-warn");
  return (
    <section className="space-y-4 rounded-lg border border-veil-edge bg-veil p-5">
      <div className="flex items-baseline justify-between gap-3">
        <p className="text-sm font-medium text-ink">On this machine</p>
        {installed ? null : measured ? (
          <span className={`text-hint font-medium ${TONE_TEXT[tone]}`}>{verdict(fit)}</span>
        ) : cap.isError ? (
          <span className="text-hint text-ink-muted">not measured</span>
        ) : (
          <span className="text-hint text-ink-muted">measuring…</span>
        )}
      </div>
      {measured && (
        <CapacityBar
          total={host.memTotal}
          tone={fit.memory.tone}
          bands={[
            { label: apps.length === 1 ? "1 app" : `${apps.length} apps`, bytes: appsMem, kind: "apps" },
            { label: "System", bytes: other, kind: "other" },
            { label: entry.title, bytes: fit.memory.add, kind: "add" },
            { label: "when busy", bytes: Math.max(0, fit.memory.addPeak - fit.memory.add), kind: "peak" },
          ]}
        />
      )}
      <dl className="space-y-2.5">
        {lines.map((l) => (
          <div key={l.label} className="flex gap-3 text-hint">
            <dt className="w-16 shrink-0 text-ink-muted">{l.label}</dt>
            <dd className="flex min-w-0 items-start gap-2 text-ink-muted">
              <span aria-hidden className={`mt-[7px] h-1.5 w-1.5 shrink-0 rounded-full ${dot(l.tone)}`} />
              <span>{l.text}</span>
            </dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

// The facts column, read off the spec rather than written twice: what the
// app runs from, what runs beside it, where its data goes, and what it
// asks of the machine.
function facts(entry: CatalogEntry): { label: string; value: ReactNode }[] {
  const spec = entry.spec;
  const mono = (s: string) => <span className="font-mono text-xs text-ink">{s}</span>;
  const out: { label: string; value: ReactNode }[] = [];
  if (entry.developer) out.push({ label: "Developer", value: entry.developer });
  if (entry.license) out.push({ label: "Licence", value: entry.license });
  out.push({ label: "Category", value: entry.category });
  if (spec.source.type === "git") {
    out.push({ label: "Source", value: "Built from a git repository" });
  } else if (spec.source.image) {
    out.push({ label: "Image", value: mono(spec.source.image) });
  }
  const aux = spec.aux ?? [];
  if (aux.length > 0) out.push({ label: "Runs with", value: aux.map((a) => a.name).join(", ") });
  if (spec.needs?.postgres) out.push({ label: "Database", value: "PostgreSQL, managed by Fog" });
  const volumes = spec.volumes ?? [];
  const libraries = volumes.filter((v) => v.files).map((v) => v.files!);
  out.push({
    label: "Storage",
    value: volumes.length === 0
      ? "Nothing on disk"
      : libraries.length > 0
        ? <>{volumes.length === libraries.length ? "" : `${volumes.length - libraries.length} own, `}reads {libraries.map((l, i) => <span key={l}>{i > 0 && ", "}{mono(l)}</span>)}</>
        : `${volumes.length} ${volumes.length === 1 ? "folder" : "folders"} of its own`,
  });
  const backup = spec.backup;
  out.push({
    label: "Backups",
    value: !backup || !backup.enabled
      ? "Off — nothing to keep"
      : backup.mode === "delegated"
        ? `Artifact the app produces${backup.beforeUpdate !== false ? ", and before updates unless turned off" : ""}`
        : `Nightly${backup.beforeUpdate !== false ? ", and before updates unless turned off" : ""}`,
  });
  const ports = spec.ports ?? [];
  out.push({
    label: "Network",
    value: ports.length === 0
      ? "Through Fog's proxy only"
      : <>Publishes {ports.map((p, i) => <span key={i}>{i > 0 && ", "}{mono(`${p.hostPort ?? p.port}/${p.protocol ?? "tcp"}`)}</span>)}</>,
  });
  if (entry.requires?.cpu) out.push({ label: "Requires", value: `${entry.requires.cpu} processor` });
  if (spec.memoryLimit) out.push({ label: "Memory", value: `Limited to ${spec.memoryLimit.toUpperCase()}` });
  return out;
}

// The strip of pictures, and the one picture large. The strip scrolls
// sideways under the page gutter; a click opens the picture at full size
// with its caption, and ← → move between them.
function Screenshots({ shots }: { shots: { src: string; alt: string }[] }) {
  const [open, setOpen] = useState<number | null>(null);

  useEffect(() => {
    if (open === null) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(null);
      else if (e.key === "ArrowRight") setOpen((i) => (i === null ? i : (i + 1) % shots.length));
      else if (e.key === "ArrowLeft") setOpen((i) => (i === null ? i : (i - 1 + shots.length) % shots.length));
      else return;
      e.preventDefault();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, shots.length]);

  return (
    <>
      <div className="-mx-4 flex gap-3 overflow-x-auto px-4 pb-2 sm:-mx-6 sm:px-6 lg:-ml-8 lg:mr-0 lg:pl-8 lg:pr-0">
        {shots.map((s, i) => (
          <button
            key={s.src}
            type="button"
            onClick={() => setOpen(i)}
            title={s.alt}
            className="shrink-0 overflow-hidden rounded-lg border border-veil-edge bg-veil transition-colors hover:border-edge-hover"
          >
            <img src={s.src} alt={s.alt} loading="lazy" className="h-56 w-auto max-w-none sm:h-72" />
          </button>
        ))}
      </div>
      {open !== null && (
        <div
          role="dialog"
          aria-modal="true"
          aria-label={shots[open].alt}
          className="fixed inset-0 z-50 flex flex-col items-center justify-center gap-3 bg-black/85 p-4"
          onClick={() => setOpen(null)}
        >
          <img
            src={shots[open].src}
            alt={shots[open].alt}
            className="max-h-[85vh] max-w-[94vw] rounded-lg border border-veil-edge object-contain"
            onClick={(e) => e.stopPropagation()}
          />
          <p className="text-sm text-ink-muted">
            {shots[open].alt}
            {shots.length > 1 && <span className="ml-3 text-ink-muted">{open + 1} of {shots.length}</span>}
          </p>
        </div>
      )}
    </>
  );
}
