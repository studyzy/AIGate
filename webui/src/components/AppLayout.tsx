import { NavLink, Outlet, useNavigate } from "react-router-dom";

import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";

import { LangSwitch } from "./LangSwitch";
import { clearToken } from "@/lib/session";

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

export function AppLayout() {
  const { t } = useI18n();
  const navigate = useNavigate();

  return (
    <div className="flex h-full">
      <aside className="flex w-60 shrink-0 flex-col border-r border-slate-200 bg-white">
        <div className="flex h-14 items-center border-b border-slate-200 px-4 font-semibold">
          {t("app.name")}
        </div>
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
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 items-center justify-end gap-4 border-b border-slate-200 bg-white px-4">
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
        </header>
        <main className="min-w-0 flex-1 overflow-y-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
