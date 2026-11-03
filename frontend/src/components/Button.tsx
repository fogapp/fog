// The same ninety-character Tailwind string appeared in twenty-odd places,
// with small unintentional divergences — disabled:opacity-50 in most, -40 in
// one, text-ink in most, -300 in another. Naming the variants makes those
// differences deliberate rather than accidental.
//
// Copy is sentence case everywhere: "Prune cache", "Add disk", "Install app".
//
// Every control in fog shares one height — 32px, 24px for a compact action
// in a dense row — so a button, a field and a select beside each other line
// up without anyone arranging it.

import type { ButtonHTMLAttributes, ReactNode } from "react";
import { Link } from "react-router-dom";

// Nothing in a house is an emergency, so a `danger` (destructive) action is
// amber-outlined, never red. `info` is the beacon-tinted action that pairs
// with the "update available" pill.
type Variant = "primary" | "secondary" | "info" | "danger" | "ghost";
type Size = "sm" | "md";

const base =
  "inline-flex shrink-0 items-center justify-center gap-1.5 whitespace-nowrap rounded-md font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-40 [&>svg]:size-4 [&>svg]:shrink-0";

const variants: Record<Variant, string> = {
  primary: "bg-accent text-accent-on hover:bg-accent-hover",
  secondary: "border border-edge bg-veil text-ink hover:border-edge-hover hover:bg-hover",
  info: "border border-accent/30 bg-accent/10 text-accent hover:bg-accent/15",
  danger: "border border-warn/45 text-warn hover:bg-warn/10",
  ghost: "text-ink-muted hover:bg-hover hover:text-ink",
};

const sizes: Record<Size, string> = {
  sm: "h-6 px-2 text-xs [&>svg]:size-3.5",
  md: "h-8 px-3.5 text-sm",
};

// The same classes for something that must wear a button but can't be one:
// a <span> inside a card that is itself the link, a <summary>.
export function buttonClass(variant: Variant = "secondary", size: Size = "md") {
  return `${base} ${variants[variant]} ${sizes[size]}`;
}

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  size?: Size;
  children: ReactNode;
}

export default function Button({
  variant = "secondary",
  size = "md",
  className = "",
  children,
  ...rest
}: ButtonProps) {
  return (
    <button className={`${buttonClass(variant, size)} ${className}`} {...rest}>
      {children}
    </button>
  );
}

// A link that wears a button: "Open Drive", "Export all app manifests".
// It goes somewhere (to, an in-app path) or fetches something (href, with
// download) rather than doing something, so it stays an <a> — but it reads
// as the same control as the buttons beside it.
export function ButtonLink({
  to,
  href,
  download,
  variant = "secondary",
  size = "md",
  className = "",
  children,
}: {
  to?: string;
  href?: string;
  download?: boolean;
  variant?: Variant;
  size?: Size;
  className?: string;
  children: ReactNode;
}) {
  const cls = `${buttonClass(variant, size)} ${className}`;
  if (to) return <Link to={to} className={cls}>{children}</Link>;
  return <a href={href} download={download} className={cls}>{children}</a>;
}
