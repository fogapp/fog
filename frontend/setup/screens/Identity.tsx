import { useState } from "react";
import Button from "../../src/components/Button";
import Field from "../../src/components/Field";

export function IdentityScreen(props: {
  serverName: string;
  setServerName: (v: string) => void;
  username: string;
  setUsername: (v: string) => void;
  password: string;
  setPassword: (v: string) => void;
  onBack: () => void;
  onDone: () => void;
}) {
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState("");
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;

  const submit = () => {
    if (!/^[a-z][a-z0-9-]{0,31}$/.test(props.serverName)) {
      setError("Server name: lowercase letters, digits, and hyphens only.");
      return;
    }
    if (!props.username.trim()) {
      setError("Pick a username.");
      return;
    }
    if (props.password.length < 10) {
      setError("Password must be at least 10 characters.");
      return;
    }
    if (props.password !== confirm) {
      setError("Passwords don't match.");
      return;
    }
    setError("");
    props.onDone();
  };

  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-[26px] font-bold leading-tight tracking-[-.015em] text-ink">Your server</h2>
        <p className="mt-1 text-sm text-ink-muted">
          The name is how you&apos;ll reach it; the account is yours alone.
        </p>
      </div>
      <Field
        label="Server name"
        value={props.serverName}
        onChange={(e) => props.setServerName(e.target.value.toLowerCase())}
        hint={`Reachable as http://${props.serverName || "fog"}.local`}
        autoCapitalize="none"
        autoCorrect="off"
      />
      <Field
        label="Username"
        value={props.username}
        onChange={(e) => props.setUsername(e.target.value)}
        autoCapitalize="none"
        autoCorrect="off"
      />
      <Field
        label="Password"
        type="password"
        value={props.password}
        onChange={(e) => props.setPassword(e.target.value)}
        hint="At least 10 characters."
      />
      <Field
        label="Confirm password"
        type="password"
        value={confirm}
        onChange={(e) => setConfirm(e.target.value)}
      />
      <p className="text-xs text-ink-muted">Time zone: {tz} (from this browser)</p>
      {error && <p className="text-sm text-warn">{error}</p>}
      <div className="flex gap-3">
        <Button className="flex-1" onClick={props.onBack}>
          Back
        </Button>
        <Button variant="primary" className="flex-1" onClick={submit}>
          Continue
        </Button>
      </div>
    </div>
  );
}
