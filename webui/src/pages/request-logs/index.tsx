import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useSearchParams } from "react-router-dom";

import type {
  ApiKey,
  RequestLogDetail,
  RequestLogFilter,
  RequestLogRow,
  RequestLogsResponse,
} from "@/api/types";
import { QueryError } from "@/components/ui/QueryError";
import type { Column } from "@/components/ui/Table";
import { Badge, Button, EmptyState, Field, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";
import { formatDateTime, formatLatency, formatNumber } from "@/lib/format";

const PAGE_SIZE = 20;

type FilterKey = keyof RequestLogFilter;
type UrlFilterKey = Exclude<FilterKey, "page" | "page_size">;

// 13 个查询参数里除分页外的 11 个：全部取自 URL，queryKey 也全部带上。
const FILTER_KEYS: UrlFilterKey[] = [
  "start",
  "end",
  "api_key_id",
  "api_key_hash",
  "model",
  "path",
  "status_code",
  "stream",
  "min_latency_ms",
  "max_latency_ms",
  "q",
];

function statusTone(code: number): "green" | "amber" | "red" | "slate" {
  if (code >= 200 && code < 300) return "green";
  if (code >= 400 && code < 500) return "amber";
  if (code >= 500) return "red";
  return "slate";
}

// 列表接口的时间边界只认 YYYY-MM-DD 或 RFC3339；datetime-local 的值是本地墙上时间，
// 按现有页面约定（见 billing/rates.tsx）转成 UTC ISO 字符串，纯日期原样透传。
function timeBound(value: string): string {
  if (!value || !value.includes("T")) return value;
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? value : d.toISOString();
}

// 正文与白名单头都可能是 JSON；能解析就缩进美化，失败则原样展示。
function pretty(text: string): string {
  if (!text) return "";
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}

function CopyButton({ text }: { text: string }) {
  const { t } = useI18n();
  const [copied, setCopied] = useState(false);
  return (
    <Button
      variant="secondary"
      onClick={() => {
        if (!navigator.clipboard) return;
        void navigator.clipboard.writeText(text).then(() => {
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1500);
        });
      }}
    >
      {copied ? t("common.copied") : t("common.copy")}
    </Button>
  );
}

function BodyBlock({ title, text }: { title: string; text: string }) {
  return (
    <div className="mb-3">
      <div className="mb-1 flex items-center justify-between gap-2">
        <span className="text-xs font-medium text-slate-500">{title}</span>
        <CopyButton text={text} />
      </div>
      <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-all rounded-md bg-slate-900 p-3 text-xs text-slate-100">
        {pretty(text) || "—"}
      </pre>
    </div>
  );
}

export function RequestLogsPage() {
  const { t } = useI18n();
  const [params, setParams] = useSearchParams();
  const rawPage = Number(params.get("page") ?? "1");
  const page = Number.isFinite(rawPage) && rawPage >= 1 ? Math.floor(rawPage) : 1;
  const [expanded, setExpanded] = useState<string | null>(null);

  // 筛选值与页码都以 URL search params 为唯一来源；空串表示该维度不过滤。
  const filter: RequestLogFilter = {
    start: params.get("start") ?? "",
    end: params.get("end") ?? "",
    api_key_id: params.get("api_key_id") ?? "",
    api_key_hash: params.get("api_key_hash") ?? "",
    model: params.get("model") ?? "",
    path: params.get("path") ?? "",
    status_code: params.get("status_code") ?? "",
    stream: params.get("stream") ?? "",
    min_latency_ms: params.get("min_latency_ms") ?? "",
    max_latency_ms: params.get("max_latency_ms") ?? "",
    q: params.get("q") ?? "",
    page: String(page),
    page_size: String(PAGE_SIZE),
  };

  const search = new URLSearchParams();
  FILTER_KEYS.forEach((key) => {
    const raw = filter[key];
    if (!raw) return;
    search.set(key, key === "start" || key === "end" ? timeBound(raw) : raw);
  });
  search.set("page", String(page));
  search.set("page_size", String(PAGE_SIZE));
  const queryString = search.toString();

  // queryKey 带整个 filter，任一筛选值变化都会重新取数。
  const logs = useQuery({
    queryKey: ["request-logs", filter],
    queryFn: () => api<RequestLogsResponse>("/admin/request-logs?" + queryString),
  });
  const keys = useQuery({
    queryKey: ["keys"],
    queryFn: () => api<{ keys: ApiKey[] }>("/admin/keys"),
  });
  const detail = useQuery({
    queryKey: ["request-log-detail", expanded],
    queryFn: () => api<RequestLogDetail>("/admin/request-logs/" + encodeURIComponent(expanded ?? "")),
    enabled: expanded !== null,
  });

  function patch(next: Record<string, string>) {
    const merged = new URLSearchParams(params);
    Object.entries(next).forEach(([k, v]) => (v === "" ? merged.delete(k) : merged.set(k, v)));
    setParams(merged, { replace: true });
  }

  // 改任一筛选都把页码重置回第一页。
  function setFilter(key: UrlFilterKey, value: string) {
    patch({ [key]: value, page: "1" });
  }

  const rows = logs.data?.logs ?? [];
  const total = logs.data?.total ?? 0;

  const columns: Column<RequestLogRow>[] = [
    { key: "time", header: t("requestLogs.time"), render: (r) => formatDateTime(r.created_at) },
    { key: "method", header: t("requestLogs.method"), render: (r) => r.method || "—" },
    { key: "path", header: t("requestLogs.path"), render: (r) => r.path || "—" },
    { key: "model", header: t("requestLogs.model"), render: (r) => r.model || "—" },
    {
      key: "key",
      header: t("requestLogs.keyHash"),
      render: (r) => <span className="font-mono text-xs">{r.api_key_hash || "—"}</span>,
    },
    {
      key: "status",
      header: t("requestLogs.statusCode"),
      render: (r) => <Badge tone={statusTone(r.status_code)}>{r.status_code}</Badge>,
    },
    { key: "latency", header: t("requestLogs.latency"), render: (r) => formatLatency(r.latency_ms), className: "tabular-nums" },
    {
      key: "stream",
      header: t("requestLogs.stream"),
      render: (r) => (r.stream ? <Badge tone="amber">stream</Badge> : null),
    },
    { key: "error", header: t("requestLogs.error"), render: (r) => r.error || "—" },
  ];

  return (
    <section>
      <PageHeader title={t("nav.requestLogs")} />

      <div className="mb-4 grid grid-cols-2 gap-3 rounded-xl border border-slate-200 bg-white p-4 md:grid-cols-4">
        <Field label={t("requestLogs.start")}>
          <Input type="datetime-local" value={filter.start} onChange={(e) => setFilter("start", e.target.value)} />
        </Field>
        <Field label={t("requestLogs.end")}>
          <Input type="datetime-local" value={filter.end} onChange={(e) => setFilter("end", e.target.value)} />
        </Field>
        <Field label={t("requestLogs.apiKey")}>
          <Select value={filter.api_key_id} onChange={(e) => setFilter("api_key_id", e.target.value)}>
            <option value="">{t("requestLogs.allKeys")}</option>
            {(keys.data?.keys ?? []).map((k) => (
              <option key={k.id} value={k.id}>
                {k.name} (#{k.id})
              </option>
            ))}
          </Select>
        </Field>
        <Field label={t("requestLogs.model")}>
          <Input value={filter.model} onChange={(e) => setFilter("model", e.target.value)} />
        </Field>
        <Field label={t("requestLogs.path")}>
          <Input value={filter.path} onChange={(e) => setFilter("path", e.target.value)} placeholder="/v1/chat/completions" />
        </Field>
        <Field label={t("requestLogs.statusCode")}>
          <Input inputMode="numeric" value={filter.status_code} onChange={(e) => setFilter("status_code", e.target.value)} />
        </Field>
        <Field label={t("requestLogs.stream")}>
          <Select value={filter.stream} onChange={(e) => setFilter("stream", e.target.value)}>
            <option value="">{t("requestLogs.allStream")}</option>
            <option value="true">{t("requestLogs.streaming")}</option>
            <option value="false">{t("requestLogs.nonStreaming")}</option>
          </Select>
        </Field>
        <Field label={t("requestLogs.minLatency")}>
          <Input type="number" min={0} value={filter.min_latency_ms} onChange={(e) => setFilter("min_latency_ms", e.target.value)} />
        </Field>
        <Field label={t("requestLogs.maxLatency")}>
          <Input type="number" min={0} value={filter.max_latency_ms} onChange={(e) => setFilter("max_latency_ms", e.target.value)} />
        </Field>
        <Field label={t("requestLogs.search")}>
          <Input value={filter.q} onChange={(e) => setFilter("q", e.target.value)} />
        </Field>
      </div>

      {logs.isError ? (
        <QueryError error={logs.error} onRetry={() => void logs.refetch()} />
      ) : logs.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : rows.length === 0 ? (
        <EmptyState title={t("requestLogs.emptyTitle")} />
      ) : (
        <>
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
                    onClick={() => setExpanded(expanded === row.id ? null : row.id)}
                    className={"cursor-pointer " + (expanded === row.id ? "bg-blue-50" : "hover:bg-slate-50")}
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
          <div className="mt-3 flex items-center justify-between text-sm text-slate-600">
            <span>
              {formatNumber(total)} · {page}/{Math.max(1, Math.ceil(total / PAGE_SIZE))}
            </span>
            <div className="flex gap-2">
              <Button variant="secondary" disabled={page <= 1} onClick={() => patch({ page: String(page - 1) })}>
                {t("requestLogs.prev")}
              </Button>
              <Button variant="secondary" disabled={page * PAGE_SIZE >= total} onClick={() => patch({ page: String(page + 1) })}>
                {t("requestLogs.next")}
              </Button>
            </div>
          </div>
        </>
      )}

      {expanded !== null ? (
        <div className="mt-3 rounded-xl border border-slate-200 bg-white p-4">
          <div className="mb-2 flex items-center gap-2">
            <span className="text-sm font-medium text-slate-700">{t("requestLogs.details")}</span>
            {detail.data?.stream ? (
              <>
                <Badge tone="amber">stream</Badge>
                <span className="text-xs text-slate-500">{t("requestLogs.streamNote")}</span>
              </>
            ) : null}
          </div>
          {detail.isLoading ? (
            <Spinner label={t("common.loading")} />
          ) : detail.isError ? (
            <QueryError error={detail.error} onRetry={() => void detail.refetch()} />
          ) : detail.data ? (
            <>
              <BodyBlock title={t("requestLogs.requestBody")} text={detail.data.request_body} />
              <BodyBlock title={t("requestLogs.responseBody")} text={detail.data.response_body} />
              <BodyBlock title={t("requestLogs.requestHeaders")} text={detail.data.request_headers} />
              <BodyBlock title={t("requestLogs.responseHeaders")} text={detail.data.response_headers} />
            </>
          ) : null}
        </div>
      ) : null}
    </section>
  );
}
