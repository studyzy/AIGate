import { useEffect } from "react";
import { Navigate, Outlet, useLocation, useNavigate } from "react-router-dom";

import { UNAUTHORIZED_EVENT } from "@/lib/api";
import { getToken } from "@/lib/session";

// AuthGuard 拦住未登录访问，并接管任意请求返回的 401。
export function AuthGuard() {
  const token = getToken();
  const location = useLocation();
  const navigate = useNavigate();

  useEffect(() => {
    const onUnauthorized = () => navigate("/login", { replace: true });
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, [navigate]);

  if (!token) return <Navigate to="/login" replace state={{ from: location }} />;
  return <Outlet />;
}
