// AddressField picks the subdomain an app is served on. It edits only the
// label — the base domain is fixed and shown as a suffix, so the field can't
// be used to type a name the wildcard certificate wouldn't cover. Shared by
// the installer (choosing the first address) and the app page (moving it).

import SettingsLink from "./SettingsLink";

export default function AddressField({ value, onChange, placeholder, baseDomain, loaded, error, label = "Address (optional)", autoFocus }: {
  value: string;
  onChange: (v: string) => void;
  placeholder: string;
  baseDomain: string;
  loaded: boolean;
  error: string | null;
  label?: string | null;
  autoFocus?: boolean;
}) {
  // Until settings land we don't know the domain, and a suffix-less box that
  // fills in later would be claiming the wrong shape.
  if (!loaded) return <p className="text-xs text-ink-muted">Loading your domain…</p>;
  if (!baseDomain) {
    return (
      <p className="text-xs text-ink-muted">
        Apps get an address once a base domain is set up in <SettingsLink to="network/domain" />. Until then this
        one is reachable on the server's LAN address, and you can choose its subdomain later
        from the app's page.
      </p>
    );
  }
  return (
    <div>
      {label && <span className="mb-1 block text-sm font-medium text-ink">{label}</span>}
      <div className={`flex items-stretch rounded-md border bg-fog-900 ${error ? "border-warn/30" : "border-edge focus-within:border-fog-400"}`}>
        <input
          value={value} onChange={(e) => onChange(e.target.value.toLowerCase())} placeholder={placeholder}
          autoCapitalize="off" autoCorrect="off" spellCheck={false} aria-label="Subdomain" autoFocus={autoFocus}
          className="min-w-0 flex-1 bg-transparent px-3 py-2 text-sm text-ink placeholder-ink-muted outline-none"
        />
        <span className="flex select-none items-center whitespace-nowrap border-l border-hair px-3 text-sm text-ink-muted">
          .{baseDomain}
        </span>
      </div>
      {error
        ? <p className="mt-1 text-xs text-warn">{error}</p>
        : <p className="mt-1 text-xs text-ink-muted">
            {value.trim()
              ? `Fog will serve this app at https://${value.trim().toLowerCase()}.${baseDomain}.`
              : `Defaults to the app's name — https://${placeholder}.${baseDomain}.`}
          </p>}
    </div>
  );
}
