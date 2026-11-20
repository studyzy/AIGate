import { createBrowserRouter, Navigate } from "react-router-dom";

import { DashboardPage } from "./pages/dashboard";
import { LoginPage } from "./pages/login";

// 路由挂在 /ui 下，basename 让链接与刷新后的直接访问都落在同一前缀内。
export const router = createBrowserRouter(
  [
    { path: "/login", element: <LoginPage /> },
    { path: "/", element: <DashboardPage /> },
    { path: "*", element: <Navigate to="/" replace /> },
  ],
  { basename: "/ui" },
);
