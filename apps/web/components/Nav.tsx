"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { clsx } from "@/lib/clsx";

interface NavItem {
  href: string;
  label: string;
  icon: string; // single glyph, rendered in mono
}

interface NavGroup {
  title: string;
  items: NavItem[];
}

const WORKSPACE: NavGroup = {
  title: "Workspace",
  items: [
    { href: "/dashboard", label: "Overview", icon: "◈" },
    { href: "/dashboard/upload", label: "Upload & Run", icon: "↥" },
    { href: "/dashboard/jobs", label: "Jobs", icon: "≣" },
    { href: "/dashboard/artifacts", label: "Artifacts", icon: "♪" },
    { href: "/dashboard/usage", label: "Usage", icon: "▟" },
  ],
};

const PLATFORM: NavGroup = {
  title: "Platform",
  items: [
    { href: "/dashboard/processors", label: "Processors", icon: "⚙" },
    { href: "/dashboard/api-keys", label: "API Keys", icon: "⚿" },
    { href: "/dashboard/webhooks", label: "Webhooks", icon: "⇄" },
    { href: "/dashboard/streaming", label: "Streaming", icon: "≈" },
  ],
};

const OPS: NavGroup = {
  title: "Operations",
  items: [
    { href: "/dashboard/ops", label: "Control Room", icon: "◉" },
    { href: "/dashboard/ops/audit", label: "Audit Log", icon: "☰" },
    { href: "/dashboard/ops/marketplace", label: "Moderation", icon: "◇" },
  ],
};

function isActive(pathname: string, href: string): boolean {
  // Overview (/dashboard) must match exactly — it's a prefix of every other route.
  if (href === "/dashboard") return pathname === "/dashboard";
  return pathname === href || pathname.startsWith(href + "/");
}

function Group({ group, pathname }: { group: NavGroup; pathname: string }) {
  return (
    <div className="mb-6">
      <div className="label mb-2 px-3">{group.title}</div>
      <ul className="space-y-0.5">
        {group.items.map((item) => {
          const active = isActive(pathname, item.href);
          return (
            <li key={item.href}>
              <Link
                href={item.href}
                className={clsx(
                  "group flex items-center gap-3 rounded-md px-3 py-2 text-sm transition-colors",
                  active
                    ? "bg-brass/10 text-brass"
                    : "text-ink-mid hover:bg-panel-2 hover:text-ink-hi",
                )}
              >
                <span
                  className={clsx(
                    "font-mono text-base leading-none",
                    active ? "text-brass" : "text-ink-lo group-hover:text-ink-mid",
                  )}
                >
                  {item.icon}
                </span>
                <span className="font-medium">{item.label}</span>
                {active && <span className="ml-auto h-1.5 w-1.5 rounded-full bg-brass" />}
              </Link>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

export function Nav({ admin }: { admin: boolean }) {
  const pathname = usePathname();
  return (
    <nav className="px-3 py-2">
      <Group group={WORKSPACE} pathname={pathname} />
      <Group group={PLATFORM} pathname={pathname} />
      {admin && <Group group={OPS} pathname={pathname} />}
    </nav>
  );
}
