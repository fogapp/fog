// How an audit verb reads at a glance: amber for what failed, was cancelled
// or removed something, the beacon for what brought something up or kept
// it safe, quiet for the rest. An OOM is its own verb segment — "room"
// is not one. Shared by the app's activity tab and the
// whole-server activity log, so a deploy is the same colour in both.
export function auditTone(action: string): "ok" | "warn" | "info" {
  if (/fail|error|cancel|delete|unpair|(^|\.)oom/.test(action) && !/errors\.resolve/.test(action)) return "warn";
  if (/^app\.(start|restart|deploy|update|create)|backup\.create|errors\.resolve|connect\.(request|serve)/.test(action)) return "ok";
  return "info";
}
