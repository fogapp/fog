// Settings › Network › Proxy. Caddy: the reverse proxy fog runs in front
// of every app, set up by the domain wizard. Here: the live container and
// the escape hatches.

import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";
import { post } from "../../api";
import { Loading, errorText } from "../../components/Async";
import Button from "../../components/Button";
import Field from "../../components/Field";
import { StatusLine } from "../../components/Section";
import { useFlash } from "../../lib/useFlash";
import { paths } from "../../lib/paths";
import ContainerFacts from "./ContainerFacts";
import { SettingsPage } from "./Frame";
import { useDomainStatus } from "./Domain";
import { useSettingsForm } from "./useSettingsForm";
import { settingsQuery } from "../../queries";

export default function Proxy() {
  const { value, set, SaveButton, loading } = useSettingsForm(["acme_email", "caddy_image"]);
  const [error, setError] = useState("");
  const [proxyNote, flashProxyNote] = useFlash();

  const applyProxy = useMutation({
    mutationFn: () => post("/api/proxy/apply"),
    onSuccess: () => {
      setError("");
      flashProxyNote("Proxy applied ✓");
    },
    onError: (e) => setError(errorText(e, "Proxy apply failed")),
  });

  // The status reports what's *stored*, not what's typed: a draft left in
  // the fields must not read as the live setting.
  const settings = useQuery(settingsQuery);
  const acme = settings.data?.acme_email ?? "";
  const image = settings.data?.caddy_image ?? "";
  // Blank fields are the normal case — the interesting fact about the proxy
  // is whether it's actually up, which the domain status already knows.
  const proxy = useDomainStatus();
  const st = proxy.data;
  const overrides = [acme && `ACME email ${acme}`, image && `image ${image}`].filter(Boolean).join(" · ");

  return (
    <SettingsPage
      description={<>Caddy, the reverse proxy that answers every hostname and holds the certificates. The <Link to={paths.settings("network/domain")} className="underline">domain wizard</Link> sets it up; these are the escape hatches.</>}
      status={
        loading || (!st && !proxy.isError) ? (
          <Loading label="Checking the proxy…" />
        ) : !st ? (
          <StatusLine>Proxy status unavailable.</StatusLine>
        ) : (
          <StatusLine tone={st.proxyRunning ? "ok" : st.domain ? "warn" : "idle"}>
            {st.proxyRunning
              ? `Running${overrides ? ` · ${overrides}` : " · stock settings"}`
              : st.domain
                ? "Not running — start it from the domain wizard."
                : "Not running — no domain configured yet."}
          </StatusLine>
        )
      }
    >
      <div className="max-w-lg space-y-4">
        <ContainerFacts which="proxy" absent="No proxy container yet — it is created when a domain is set up." />
        {/* Blank inputs read as "not configured" — these fields stay out of
            sight until they can show what's actually stored. */}
        {loading ? (
          <Loading label="Loading settings…" />
        ) : (
          <>
            <Field label="ACME email (optional)" value={value("acme_email")} onChange={set("acme_email")} placeholder="you@example.com" />
            <Field label="Caddy image" value={value("caddy_image")} onChange={set("caddy_image")}
              placeholder="ghcr.io/caddybuilds/caddy-cloudflare:latest" />
          </>
        )}
        {error && <p className="text-sm text-warn">{error}</p>}
        <div className="flex flex-wrap items-end gap-3">
          {SaveButton}
          {/* The tick is transient: it reports what just happened, not a
              standing condition. */}
          <Button onClick={() => applyProxy.mutate()} disabled={applyProxy.isPending}>
            {applyProxy.isPending ? "Applying…" : proxyNote || "Apply proxy now"}
          </Button>
        </div>
      </div>
    </SettingsPage>
  );
}

