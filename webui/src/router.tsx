import { lazy, Suspense, type ComponentType, type ReactElement } from "react";
import { createBrowserRouter, Navigate } from "react-router-dom";

import { AppLayout } from "./components/AppLayout";
import { AuthGuard } from "./components/AuthGuard";
import { RouteFallback } from "./components/RouteFallback";
import { LoginPage } from "./pages/login";

// 除登录页外，页面按路由懒加载，避免首屏把全部管理页与图表库打进主 chunk。
const DashboardPage = lazy(() =>
  import("./pages/dashboard").then((m) => ({ default: m.DashboardPage })),
);
const UsagePage = lazy(() =>
  import("./pages/usage").then((m) => ({ default: m.UsagePage })),
);
const RequestLogsPage = lazy(() =>
  import("./pages/request-logs").then((m) => ({ default: m.RequestLogsPage })),
);
const ProvidersPage = lazy(() =>
  import("./pages/providers").then((m) => ({ default: m.ProvidersPage })),
);
const KeysPage = lazy(() =>
  import("./pages/keys").then((m) => ({ default: m.KeysPage })),
);
const UsersPage = lazy(() =>
  import("./pages/users").then((m) => ({ default: m.UsersPage })),
);
const GroupsPage = lazy(() =>
  import("./pages/groups").then((m) => ({ default: m.GroupsPage })),
);
const SettingsPage = lazy(() =>
  import("./pages/settings").then((m) => ({ default: m.SettingsPage })),
);
const WebhooksPage = lazy(() =>
  import("./pages/webhooks").then((m) => ({ default: m.WebhooksPage })),
);
const ModelRouterPage = lazy(() =>
  import("./pages/routers").then((m) => ({ default: m.ModelRouterPage })),
);
const BillingCalendarPage = lazy(() =>
  import("./pages/billing/calendar").then((m) => ({ default: m.BillingCalendarPage })),
);
const CurrencyRatePage = lazy(() =>
  import("./pages/billing/rates").then((m) => ({ default: m.CurrencyRatePage })),
);
const ImportPage = lazy(() =>
  import("./pages/import").then((m) => ({ default: m.ImportPage })),
);

// 用 Suspense 只包住页面内容，侧边栏等布局在切换时保持可见。
function page(Component: ComponentType): ReactElement {
  return (
    <Suspense fallback={<RouteFallback />}>
      <Component />
    </Suspense>
  );
}

// 路由挂在 /ui 下，basename 让链接与刷新后的直接访问都落在同一前缀内。
export const router = createBrowserRouter(
  [
    { path: "/login", element: <LoginPage /> },
    {
      element: <AuthGuard />,
      children: [
        {
          element: <AppLayout />,
          children: [
            { index: true, element: page(DashboardPage) },
            { path: "usage", element: page(UsagePage) },
            { path: "request-logs", element: page(RequestLogsPage) },
            { path: "providers", element: page(ProvidersPage) },
            { path: "model-routers", element: page(ModelRouterPage) },
            { path: "billing-calendar", element: page(BillingCalendarPage) },
            { path: "currency-rates", element: page(CurrencyRatePage) },
            { path: "models", element: <Navigate to="/providers" replace /> },
            { path: "keys", element: page(KeysPage) },
            { path: "users", element: page(UsersPage) },
            { path: "groups", element: page(GroupsPage) },
            { path: "settings", element: page(SettingsPage) },
            { path: "webhooks", element: page(WebhooksPage) },
            { path: "import", element: page(ImportPage) },
          ],
        },
      ],
    },
    { path: "*", element: <Navigate to="/" replace /> },
  ],
  { basename: "/ui" },
);
