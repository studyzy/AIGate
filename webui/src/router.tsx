import { createBrowserRouter, Navigate } from "react-router-dom";

import { AppLayout } from "./components/AppLayout";
import { AuthGuard } from "./components/AuthGuard";
import { DashboardPage } from "./pages/dashboard";
import { GroupsPage } from "./pages/groups";
import { ImportPage } from "./pages/import";
import { KeysPage } from "./pages/keys";
import { LoginPage } from "./pages/login";
import { ProvidersPage } from "./pages/providers";
import { SettingsPage } from "./pages/settings";
import { UsagePage } from "./pages/usage";
import { UsersPage } from "./pages/users";
import { WebhooksPage } from "./pages/webhooks";

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
            { index: true, element: <DashboardPage /> },
            { path: "usage", element: <UsagePage /> },
            { path: "providers", element: <ProvidersPage /> },
            { path: "models", element: <Navigate to="/providers" replace /> },
            { path: "keys", element: <KeysPage /> },
            { path: "users", element: <UsersPage /> },
            { path: "groups", element: <GroupsPage /> },
            { path: "settings", element: <SettingsPage /> },
            { path: "webhooks", element: <WebhooksPage /> },
            { path: "import", element: <ImportPage /> },
          ],
        },
      ],
    },
    { path: "*", element: <Navigate to="/" replace /> },
  ],
  { basename: "/ui" },
);
