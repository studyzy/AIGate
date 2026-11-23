import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { CartesianGrid, Legend, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";

import type { AIGateModel, ApiKey, UsageSummary, UsageSummaryRow, User } from "@/api/types";
import { EmptyState, PageHeader, Spinner } from "@/components/ui/primitives";
import { Card, Select } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";
import { formatMicros, formatNumber, todayISO } from "@/lib/format";

type Range = "today" | "7d" | "30d";

export function DashboardPage() {
  const { t } = useI18n();
  const [range, setRange] = useState<Range>("7d");

  const days = range === "today" ? 1 : range === "7d" ? 7 : 30;
  const start = todayISO(1 - days);
  const end = todayISO();

  const summary = useQuery({
    queryKey: ["usage-summary", "date", start, end],
    queryFn: () =>
      api<UsageSummary>("/admin/usage/summary?group_by=date&start=" + start + "&end=" + end),
  });
  const byModel = useQuery({
    queryKey: ["usage-summary", "model", start, end],
    queryFn: () =>
      api<{ rows: UsageSummaryRow[] }>("/admin/usage/summary?group_by=model&start=" + start + "&end=" + end),
  });
  const byUser = useQuery({
    queryKey: ["usage-summary", "user", start, end],
    queryFn: () =>
      api<{ rows: UsageSummaryRow[] }>("/admin/usage/summary?group_by=user&start=" + start + "&end=" + end),
  });
  const models = useQuery({ queryKey: ["models"], queryFn: () => api<{ models: AIGateModel[] }>("/admin/models") });
  const keys = useQuery({ queryKey: ["keys"], queryFn: () => api<{ keys: ApiKey[] }>("/admin/keys") });
  const users = useQuery({ queryKey: ["users"], queryFn: () => api<{ users: User[] }>("/admin/users") });

  const rows = summary.data?.rows ?? [];
  const totals = useMemo(
    () =>
      rows.reduce(
        (acc, r) => ({
          requests: acc.requests + r.request_count,
          tokens: acc.tokens + r.total_tokens,
        }),
        { requests: 0, tokens: 0 },
      ),
    [rows],
  );

  const chartData = rows.map((r) => ({
    date: r.date ? new Date(r.date).toLocaleDateString(undefined, { month: "short", day: "numeric" }) : "",
    requests: r.request_count,
    tokens: r.total_tokens,
  }));

  const modelNames = useMemo(() => {
    const map = new Map<number, string>();
    (models.data?.models ?? []).forEach((m) => map.set(m.id, m.model_name || m.model_id));
    return map;
  }, [models.data]);

  const userNameByID = useMemo(() => {
    const map = new Map<number, string>();
    (users.data?.users ?? []).forEach((u) => map.set(u.id, u.username));
    return map;
  }, [users.data]);

  const topModels = (byModel.data?.rows ?? [])
    .slice()
    .sort((a, b) => b.total_tokens - a.total_tokens)
    .slice(0, 5);

  const topUsers = (byUser.data?.rows ?? [])
    .slice()
    .sort((a, b) => b.total_tokens - a.total_tokens)
    .slice(0, 5);

  const loading = summary.isLoading || models.isLoading || keys.isLoading || users.isLoading;
  const failed = summary.error ?? byModel.error ?? byUser.error ?? models.error ?? keys.error ?? users.error;

  return (
    <section>
      <PageHeader
        title={t("nav.dashboard")}
        actions={
          <span className="w-40">
          <Select value={range} onChange={(e) => setRange(e.target.value as Range)}>
            <option value="today">{t("dashboard.range.today")}</option>
            <option value="7d">{t("dashboard.range.7d")}</option>
            <option value="30d">{t("dashboard.range.30d")}</option>
          </Select>
          </span>
        }
      />
      {failed ? <p className="text-sm text-red-600">{failed.message}</p> : null}
      {loading ? (
        <Spinner label={t("common.loading")} />
      ) : (models.data?.models.length ?? 0) === 0 ? (
        <EmptyState
          title={t("dashboard.emptyTitle")}
          description={t("dashboard.emptyHint")}
          action={
            <Link className="text-sm font-medium text-blue-600 hover:underline" to="/providers">
              {t("provider.new")}
            </Link>
          }
        />
      ) : (
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <Card title={t("dashboard.requests")} value={formatNumber(totals.requests)} />
            <Card title={t("dashboard.tokens")} value={formatNumber(totals.tokens)} />
            <Card title={t("dashboard.models")} value={formatNumber(models.data?.models.length ?? 0)} />
            <Card title={t("dashboard.keys")} value={formatNumber(keys.data?.keys.length ?? 0)} />
            {/* 本期花费：结算币金额在记账时已统一，按日期维度聚合可直接求和。 */}
            <Card title={t("dashboard.cost")} value={formatMicros(summary.data?.total_cost_platform_micros ?? 0, summary.data?.platform_currency ?? "")} valueTestId="dashboard-cost" />
          </div>

          <div className="rounded-xl border border-slate-200 bg-white p-4">
            <h2 className="mb-3 text-sm font-medium text-slate-700">{t("dashboard.trend")}</h2>
            <div className="h-64" data-testid="dashboard-chart">
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={chartData}>
                  <CartesianGrid strokeDasharray="3 3" stroke="#e2e8f0" />
                  <XAxis dataKey="date" fontSize={12} />
                  <YAxis fontSize={12} />
                  <Tooltip />
                  <Legend />
                  <Line type="monotone" dataKey="requests" name={t("dashboard.requests")} stroke="#2563eb" dot={false} />
                  <Line type="monotone" dataKey="tokens" name={t("dashboard.tokens")} stroke="#10b981" dot={false} />
                </LineChart>
              </ResponsiveContainer>
            </div>
          </div>

          <div className="grid gap-4 md:grid-cols-2">
            <div className="rounded-xl border border-slate-200 bg-white p-4">
              <h2 className="mb-3 text-sm font-medium text-slate-700">{t("dashboard.topModels")}</h2>
              {topModels.length === 0 ? (
                <p className="text-sm text-slate-500">{t("common.empty")}</p>
              ) : (
                <ul className="divide-y divide-slate-100 text-sm">
                  {topModels.map((row) => (
                    <li key={row.ai_model_id} className="flex items-center justify-between py-2">
                      <span>{modelNames.get(row.ai_model_id ?? 0) ?? "#" + row.ai_model_id}</span>
                      <span className="tabular-nums text-slate-600">{formatNumber(row.total_tokens)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            <div className="rounded-xl border border-slate-200 bg-white p-4">
              <h2 className="mb-3 text-sm font-medium text-slate-700">{t("dashboard.topUsers")}</h2>
              {topUsers.length === 0 ? (
                <p className="text-sm text-slate-500">{t("common.empty")}</p>
              ) : (
                <ul className="divide-y divide-slate-100 text-sm">
                  {topUsers.map((row) => (
                    <li key={row.user_id} className="flex items-center justify-between py-2">
                      <span>{userNameByID.get(row.user_id ?? 0) ?? "#" + row.user_id}</span>
                      <span className="tabular-nums text-slate-600">{formatNumber(row.total_tokens)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>
        </div>
      )}
    </section>
  );
}
