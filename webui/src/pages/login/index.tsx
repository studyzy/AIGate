import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";

import { Brand } from "@/components/Brand";
import { LangSwitch } from "@/components/LangSwitch";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";
import { clearToken, setToken } from "@/lib/session";

export function LoginPage() {
  const { t } = useI18n();
  const navigate = useNavigate();
  const [token, setValue] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError("");
    setBusy(true);
    // 先用候选 token 打一次只读接口校验，失败即丢弃。
    setToken(token.trim());
    try {
      await api("/admin/settings");
      navigate("/", { replace: true });
    } catch (err) {
      clearToken();
      setError(err instanceof Error && err.message !== "unauthorized" ? err.message : t("login.invalid"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex h-full flex-col items-center justify-center gap-6">
      <div className="absolute right-4 top-4">
        <LangSwitch />
      </div>
      <form
        className="w-full max-w-sm rounded-xl border border-slate-200 bg-white p-6 shadow-sm"
        onSubmit={onSubmit}
      >
        <div className="mb-5">
          <Brand name={t("app.name")} size="lg" />
        </div>
        <h1 className="text-lg font-semibold">{t("login.title")}</h1>
        <label className="mt-5 block text-sm font-medium text-slate-700" htmlFor="token">
          {t("login.token")}
        </label>
        <input
          id="token"
          type="password"
          autoComplete="off"
          autoFocus
          className="mt-1 w-full rounded-md border border-slate-300 px-3 py-2 text-sm outline-none focus:border-blue-500"
          value={token}
          onChange={(e) => setValue(e.target.value)}
        />
        {error ? <p className="mt-2 text-sm text-red-600">{error}</p> : null}
        <button
          type="submit"
          disabled={busy || token.trim() === ""}
          className="mt-5 w-full rounded-md bg-blue-600 px-3 py-2 text-sm font-medium text-white disabled:opacity-50"
        >
          {t("login.submit")}
        </button>
      </form>
    </div>
  );
}
