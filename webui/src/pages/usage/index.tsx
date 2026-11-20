import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useSearchParams } from "react-router-dom";

import type { AIGateModel, ApiKey, GroupBy, UsageLogRow, UsageSummaryRow, User, UserGroup } from "@/api/types";
import { Table, type Column } from "@/components/ui/Table";
import { Badge, Button, EmptyState, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api, downloadFile } from "@/lib/api";
import { formatDateTime, formatLatency, formatNumber, todayISO } from "@/lib/format";

const PAGE_SIZE = 20;

// 名称映射命中时显示名称，未命中回退 "#id"。
function nameOrId(names: Map<number, string>, id?: number | null): string {
  const key = id ?? 0;
  return names.get(key) || "#" + key;
}

function idText(id: number, name: string | undefined): string {
  return name ? name + " (#" + id + ")" : "#" + id;
}

export function UsagePage() {
  const { t } = useI18n();
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") === "logs" ? "logs" : "summary";
  const groupBy = (params.get("group_by") ?? "date") as GroupBy;
  const start = params.get("start") ?? todayISO(-6);
  const end = params.get("end") ?? todayISO();
  const page = Number(params.get("page") ?? "1");
  const [expanded, setExpanded] = useState<number | null>(null);

  function patch(next: Record<string, string>) {
    const merged = new URLSearchParams(params);
    Object.entries(next).forEach(([k, v]) => (v === "" ? merged.delete(k) : merged.set(k, v)));
    setParams(merged, { replace: true });
  }

  const models = useQuery({ queryKey: ["models"], queryFn: () => api<{ models: AIGateModel[] }>("/admin/models") });
  const modelNames = new Map<number, string>();
  (models.data?.models ?? []).forEach((m) => modelNames.set(m.id, m.model_name || m.model_id));

  const keys = useQuery({ queryKey: ["keys"], queryFn: () => api<{ keys: ApiKey[] }>("/admin/keys") });
  const users = useQuery({ queryKey: ["users"], queryFn: () => api<{ users: User[] }>("/admin/users") });
  const groups = useQuery({ queryKey: ["groups"], queryFn: () => api<{ user_groups: UserGroup[] }>("/admin/user-groups") });

  const keyNames = new Map<number, string>();
  (keys.data?.keys ?? []).forEach((k) => keyNames.set(k.id, k.name));
  const userNames = new Map<number, string>();
  (users.data?.users ?? []).forEach((u) => userNames.set(u.id, u.username));
  const groupNames = new Map<number, string>();
  (groups.data?.user_groups ?? []).forEach((g) => groupNames.set(g.id, g.name));

  const filter = "start=" + start + "&end=" + end;

  return (
    <section>
      <PageHeader
        title={t("nav.usage")}
        actions={
          <>
            <span className="w-36">
              <Input type="date" value={start} onChange={(e) => patch({ start: e.target.value, page: "1" })} />
            </span>
            <span className="w-36">
              <Input type="date" value={end} onChange={(e) => patch({ end: e.target.value, page: "1" })} />
            </span>
            <Button
              variant="secondary"
              onClick={() => downloadFile("/admin/usage/export?format=csv&" + filter, "usage-" + start + "_" + end + ".csv")}
            >
              {t("usage.export")}
            </Button>
          </>
        }
      />

      <div className="mb-3 flex gap-2 border-b border-slate-200">
        {(
          [
            ["summary", t("usage.summary")],
            ["logs", t("usage.logs")],
          ] as const
        ).map(([key, label]) => (
          <button
            key={key}
            type="button"
            className={
              "-mb-px border-b-2 px-3 py-2 text-sm " +
              (tab === key ? "border-blue-600 font-medium text-blue-700" : "border-transparent text-slate-500")
            }
            onClick={() => patch({ tab: key })}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "summary" ? (
        <SummaryTab
          groupBy={groupBy}
          filter={filter}
          modelNames={modelNames}
          keyNames={keyNames}
          userNames={userNames}
          groupNames={groupNames}
          onChange={(next) => patch({ group_by: next, page: "1" })}
        />
      ) : (
        <LogsTab
          filter={filter}
          page={page}
          onPage={(next) => patch({ page: String(next) })}
          expanded={expanded}
          onExpand={setExpanded}
          keyNames={keyNames}
          userNames={userNames}
          groupNames={groupNames}
        />
      )}
    </section>
  );
}

function SummaryTab({
  groupBy,
  filter,
  modelNames,
  keyNames,
  userNames,
  groupNames,
  onChange,
}: {
  groupBy: GroupBy;
  filter: string;
  modelNames: Map<number, string>;
  keyNames: Map<number, string>;
  userNames: Map<number, string>;
  groupNames: Map<number, string>;
  onChange: (next: string) => void;
}) {
  const { t } = useI18n();
  const query = useQuery({
    queryKey: ["usage-summary", groupBy, filter],
    queryFn: () => api<{ rows: UsageSummaryRow[] }>("/admin/usage/summary?group_by=" + groupBy + "&" + filter),
  });

  const label = (row: UsageSummaryRow): string => {
    switch (groupBy) {
      case "date":
        return row.date ? new Date(row.date).toLocaleDateString(undefined, { dateStyle: "medium" }) : "—";
      case "model":
        return modelNames.get(row.ai_model_id ?? 0) ?? "#" + (row.ai_model_id ?? 0);
      case "key":
        return nameOrId(keyNames, row.api_key_id);
      case "user":
        return nameOrId(userNames, row.user_id);
      default:
        return nameOrId(groupNames, row.group_id);
    }
  };

  const columns: Column<UsageSummaryRow>[] = [
    { key: "label", header: t(("usage.group." + groupBy) as "usage.group.date"), render: label },
    { key: "requests", header: t("usage.requests"), render: (r) => formatNumber(r.request_count), className: "tabular-nums" },
    { key: "prompt", header: t("usage.prompt"), render: (r) => formatNumber(r.prompt_tokens), className: "tabular-nums" },
    { key: "completion", header: t("usage.completion"), render: (r) => formatNumber(r.completion_tokens), className: "tabular-nums" },
    { key: "total", header: t("usage.total"), render: (r) => formatNumber(r.total_tokens), className: "tabular-nums" },
    { key: "read", header: t("usage.cacheRead"), render: (r) => formatNumber(r.prompt_cache_read_tokens), className: "tabular-nums" },
  ];

  return (
    <div>
      <label className="mb-3 flex items-center gap-2 text-sm text-slate-500">
        {t("usage.groupBy")}
        <span className="w-36">
        <Select value={groupBy} onChange={(e) => onChange(e.target.value)}>
          {(["date", "model", "key", "user", "group"] as const).map((g) => (
            <option key={g} value={g}>
              {t(("usage.group." + g) as "usage.group.date")}
            </option>
          ))}
        </Select>
        </span>
      </label>
      {query.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table
          columns={columns}
          rows={query.data?.rows ?? []}
          rowKey={(r) => label(r)}
          empty={<EmptyState title={t("usage.emptyTitle")} />}
        />
      )}
    </div>
  );
}

function LogsTab({
  filter,
  page,
  onPage,
  expanded,
  onExpand,
  keyNames,
  userNames,
  groupNames,
}: {
  filter: string;
  page: number;
  onPage: (next: number) => void;
  expanded: number | null;
  onExpand: (id: number | null) => void;
  keyNames: Map<number, string>;
  userNames: Map<number, string>;
  groupNames: Map<number, string>;
}) {
  const { t } = useI18n();
  const query = useQuery({
    queryKey: ["usage-logs", filter, page],
    queryFn: () =>
      api<{ logs: UsageLogRow[]; total: number; page: number; page_size: number }>(
        "/admin/usage/logs?" + filter + "&page=" + page + "&page_size=" + PAGE_SIZE,
      ),
  });

  const columns: Column<UsageLogRow>[] = [
    { key: "time", header: t("usage.time"), render: (r) => formatDateTime(r.created_at) },
    { key: "model", header: t("usage.model"), render: (r) => r.model || "—" },
    { key: "provider", header: t("usage.provider"), render: (r) => r.provider || "—" },
    {
      key: "status",
      header: t("usage.status"),
      render: (r) => (
        <Badge tone={r.status_code >= 200 && r.status_code < 300 ? "green" : "red"}>{r.status_code}</Badge>
      ),
    },
    { key: "latency", header: t("usage.latency"), render: (r) => formatLatency(r.latency), className: "tabular-nums" },
    { key: "prompt", header: t("usage.prompt"), render: (r) => formatNumber(r.prompt_tokens), className: "tabular-nums" },
    { key: "completion", header: t("usage.completion"), render: (r) => formatNumber(r.completion_tokens), className: "tabular-nums" },
    { key: "total", header: t("usage.total"), render: (r) => formatNumber(r.total_tokens), className: "tabular-nums" },
  ];

  const rows = query.data?.logs ?? [];
  const total = query.data?.total ?? 0;

  const toggle = (id: number) => onExpand(expanded === id ? null : id);

  return (
    <div>
      {query.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <>
          {rows.length === 0 ? (
            <EmptyState title={t("usage.emptyTitle")} />
          ) : (
            <div className="overflow-x-auto rounded-xl border border-slate-200 bg-white">
              <table className="min-w-full divide-y divide-slate-200 text-sm">
                <thead className="bg-slate-50 text-left text-xs font-medium uppercase tracking-wide text-slate-500">
                  <tr>
                    {columns.map((c) => (
                      <th key={c.key} className={"px-4 py-2 " + (c.className ?? "")}>
                        {c.header}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-100">
                  {rows.map((row) => (
                    <tr
                      key={row.id}
                      onClick={() => toggle(row.id)}
                      className={
                        "cursor-pointer " + (expanded === row.id ? "bg-blue-50" : "hover:bg-slate-50")
                      }
                    >
                      {columns.map((c) => (
                        <td key={c.key} className={"px-4 py-2 align-middle " + (c.className ?? "")}>
                          {c.render(row)}
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {expanded !== null ? (
            <LogDetail
              row={rows.find((r) => r.id === expanded)}
              keyNames={keyNames}
              userNames={userNames}
              groupNames={groupNames}
            />
          ) : null}
          {rows.length > 0 ? (
            <div className="mt-3 flex items-center justify-between text-sm text-slate-600">
              <span>
                {formatNumber(total)} · {page}/{Math.max(1, Math.ceil(total / PAGE_SIZE))}
              </span>
              <div className="flex gap-2">
                <Button variant="secondary" disabled={page <= 1} onClick={() => onPage(page - 1)}>
                  {t("usage.prev")}
                </Button>
                <Button variant="secondary" disabled={page * PAGE_SIZE >= total} onClick={() => onPage(page + 1)}>
                  {t("usage.next")}
                </Button>
              </div>
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}

function LogDetail({
  row,
  keyNames,
  userNames,
  groupNames,
}: {
  row?: UsageLogRow;
  keyNames: Map<number, string>;
  userNames: Map<number, string>;
  groupNames: Map<number, string>;
}) {
  const { t } = useI18n();
  if (!row) return null;
  return (
    <div className="mt-3 rounded-xl border border-slate-200 bg-white p-4 text-sm">
      <div className="mb-2 font-medium text-slate-700">{t("usage.details")}</div>
      <div className="grid grid-cols-2 gap-2 md:grid-cols-5">
        <div>
          <div className="text-xs text-slate-500">{t("usage.group.key")}</div>
          {idText(row.api_key_id, keyNames.get(row.api_key_id))}
        </div>
        <div>
          <div className="text-xs text-slate-500">{t("usage.group.user")}</div>
          {idText(row.user_id, userNames.get(row.user_id))}
        </div>
        <div>
          <div className="text-xs text-slate-500">{t("usage.group.group")}</div>
          {idText(row.group_id, groupNames.get(row.group_id))}
        </div>
        <div>
          <div className="text-xs text-slate-500">{t("usage.cacheRead")}</div>
          {formatNumber(row.prompt_cache_read_tokens)}
        </div>
        <div>
          <div className="text-xs text-slate-500">{t("usage.cacheWrite")}</div>
          {formatNumber(row.prompt_cache_write_tokens)}
        </div>
      </div>
    </div>
  );
}
