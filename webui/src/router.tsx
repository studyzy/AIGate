import { createBrowserRouter, Navigate } from "react-router-dom";

import { AppLayout } from "./components/AppLayout";
import { AuthGuard } from "./components/AuthGuard";
import { Placeholder } from "./components/Placeholder";
import { DashboardPage } from "./pages/dashboard";
import { LoginPage } from "./pages/login";

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
            { path: "usage", element: <Placeholder title="nav.usage" /> },
            { path: "models", element: <Placeholder title="nav.models" /> },
            { path: "credentials", element: <Placeholder title="nav.credentials" /> },
            { path: "keys", element: <Placeholder title="nav.keys" /> },
            { path: "users", element: <Placeholder title="nav.users" /> },
            { path: "groups", element: <Placeholder title="nav.groups" /> },
            { path: "settings", element: <Placeholder title="nav.settings" /> },
            { path: "webhooks", element: <Placeholder title="nav.webhooks" /> },
            { path: "import", element: <Placeholder title="nav.import" /> },
          ],
        },
      ],
    },
    { path: "*", element: <Navigate to="/" replace /> },
  ],
  { basename: "/ui" },
);
