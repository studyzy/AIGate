import { useEffect, useState } from "react";
import { NavLink, Outlet, useNavigate } from "react-router-dom";

import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";

import { Brand } from "./Brand";
import { ErrorBoundary } from "./ErrorBoundary";
import { useModalA11y } from "./ui/useModalA11y";
import { LangSwitch } from "./LangSwitch";
import { clearToken } from "@/lib/session";

const SIDEBAR_KEY = "aigate.ui.sidebar";

const GROUPS: { title: MessageKey; items: { key: MessageKey; to: string }[] }[] = [
  {
    title: "nav.monitor",
    items: [
      { key: "nav.dashboard", to: "/" },
      { key: "nav.usage", to: "/usage" },
    ],
  },
  {
    title: "nav.resources",
    items: [
      { key: "nav.providers", to: "/providers" },
    ],
  },
  {
    title: "nav.access",
    items: [
      { key: "nav.keys", to: "/keys" },
      { key: "nav.users", to: "/users" },
      { key: "nav.groups", to: "/groups" },
    ],
  },
  {
    title: "nav.system",
    items: [
      { key: "nav.settings", to: "/settings" },
      { key: "nav.webhooks", to: "/webhooks" },
      { key: "nav.import", to: "/import" },
    ],
  },
];

// readCollapsed 读取侧栏折叠偏好，仅 "collapsed" 视为收起。
function readCollapsed(): boolean {
  if (typeof localStorage === "undefined") return false;
  return localStorage.getItem(SIDEBAR_KEY) === "collapsed";
}

function SidebarNavLinks({ onNavigate }: { onNavigate?: () => void }) {
  const { t } = useI18n();
  return (
    <nav className="flex-1 overflow-y-auto p-3">
      {GROUPS.map((group) => (
        <div key={group.title} className="mb-4">
          <div className="px-2 pb-1 text-xs font-medium uppercase tracking-wide text-slate-400">
            {t(group.title)}
          </div>
          {group.items.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === "/"}
              onClick={onNavigate}
              className={({ isActive }) =>
                "block rounded-md px-2 py-1.5 text-sm " +
                (isActive ? "bg-blue-50 font-medium text-blue-700" : "text-slate-700 hover:bg-slate-50")
              }
            >
              {t(item.key)}
            </NavLink>
          ))}
        </div>
      ))}
    </nav>
  );
}

function PanelIcon() {
  return (
    <svg
      viewBox="0 0 20 20"
      aria-hidden="true"
      className="h-4 w-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <rect x="2.5" y="3.5" width="15" height="13" rx="2" />
      <path d="M7.5 3.5v13" />
    </svg>
  );
}

function HamburgerIcon() {
  return (
    <svg
      viewBox="0 0 20 20"
      aria-hidden="true"
      className="h-4 w-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
    >
      <path d="M3 5.5h14M3 10h14M3 14.5h14" />
    </svg>
  );
}

export function AppLayout() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const [collapsed, setCollapsed] = useState(readCollapsed);
  const [mobileOpen, setMobileOpen] = useState(false);

  useEffect(() => {
    localStorage.setItem(SIDEBAR_KEY, collapsed ? "collapsed" : "open");
  }, [collapsed]);

  const closeMobile = () => setMobileOpen(false);
  const mobileRef = useModalA11y<HTMLDivElement>(mobileOpen, closeMobile);

  return (
    <div className="flex h-full">
      {collapsed ? null : (
        <aside className="hidden w-60 shrink-0 flex-col border-r border-slate-200 bg-white md:flex">
          <div className="flex h-14 items-center border-b border-slate-200 px-4">
            <Brand name={t("app.name")} />
          </div>
          <SidebarNavLinks />
        </aside>
      )}

      {mobileOpen ? (
        <div className="fixed inset-0 z-40 flex md:hidden">
          <aside
            ref={mobileRef}
            role="dialog"
            aria-modal="true"
            aria-label={t("nav.menu")}
            tabIndex={-1}
            className="flex w-60 shrink-0 flex-col border-r border-slate-200 bg-white shadow-xl outline-none"
          >
            <div className="flex h-14 items-center border-b border-slate-200 px-4">
              <Brand name={t("app.name")} />
            </div>
            <SidebarNavLinks onNavigate={closeMobile} />
          </aside>
          <div
            aria-hidden="true"
            className="flex-1 bg-slate-900/30"
            onClick={closeMobile}
          />
        </div>
      ) : null}

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 items-center justify-between gap-4 border-b border-slate-200 bg-white px-4">
          <div className="flex items-center gap-1">
            <button
              type="button"
              aria-label={t("nav.menu")}
              aria-expanded={mobileOpen}
              className="rounded-md p-2 text-slate-600 hover:bg-slate-50 md:hidden"
              onClick={() => setMobileOpen(true)}
            >
              <HamburgerIcon />
            </button>
            <button
              type="button"
              aria-label={collapsed ? t("nav.expand") : t("nav.collapse")}
              className="hidden rounded-md p-2 text-slate-600 hover:bg-slate-50 md:inline-flex"
              onClick={() => setCollapsed((v) => !v)}
            >
              <PanelIcon />
            </button>
          </div>
          <div className="flex items-center gap-4">
            <LangSwitch />
            <button
              type="button"
              className="rounded-md px-2 py-1 text-sm text-slate-600 hover:bg-slate-50"
              onClick={() => {
                clearToken();
                navigate("/login", { replace: true });
              }}
            >
              {t("nav.logout")}
            </button>
          </div>
        </header>
        <main className="min-w-0 flex-1 overflow-y-auto p-6">
          <ErrorBoundary>
            <Outlet />
          </ErrorBoundary>
        </main>
      </div>
    </div>
  );
}
