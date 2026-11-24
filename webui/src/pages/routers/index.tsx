import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import type {
  ID,
  ModelRouter,
  ModelRouterInput,
  RouterPreview,
  RouterScope,
  RouterStrategy,
  RouterStrategyField,
} from "@/api/types";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { QueryError } from "@/components/ui/QueryError";
import { Table, type Column } from "@/components/ui/Table";
import { useToast } from "@/components/ui/Toast";
import { Button, EmptyState, Field, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";

// SCOPES 与后端 model.ValidRouterScope 对齐；顺序即匹配优先级（具体 → 宽泛）。
const SCOPES: RouterScope[] = ["key", "user", "group", "global"];

// SCOPE_LABEL 是作用域到文案 key 的显式映射：i18n 的 t() 只接受字面量 key，
// 拼接字符串会绕过类型检查，漏译在编译期就发现不了。
const SCOPE_LABEL = {
  key: "routers.scope.key",
  user: "routers.scope.user",
  group: "routers.scope.group",
  global: "routers.scope.global",
} as const;

// 空表单：新建时的默认值（global 作用域、决策策略以外的字段由 strategy 决定）。
function emptyForm(strategies: RouterStrategy[]): ModelRouterInput {
  const first = strategies[0];
  return {
    name: "",
    description: "",
    strategy: first?.name ?? "",
    config: JSON.stringify(defaultConfig(first)),
    scope: "global",
    scope_id: 0,
    priority: 0,
    enabled: true,
  };
}

// defaultConfig 用策略下发的字段默认值拼出一个初始配置。
function defaultConfig(strategy?: RouterStrategy): Record<string, unknown> {
  const cfg: Record<string, unknown> = {};
  for (const f of strategy?.fields ?? []) {
    if (f.default !== undefined) cfg[f.key] = f.default;
    else if (f.type === "bool") cfg[f.key] = false;
    else if (f.type === "int") cfg[f.key] = 0;
    else cfg[f.key] = "";
  }
  return cfg;
}

/**
 * ModelRouterPage 是模型路由管理页：一张规则列表 + 新建/编辑抽屉 + 预览。
 *
 * 表单不硬编码任何策略：策略与字段由 GET /admin/model-routers/strategies 下发，
 * 新增策略时本页无需改动（只要字段类型在 SUPPORTED 之内）。
 */
export function ModelRouterPage() {
  const { t } = useI18n();
  const toast = useToast();
  const client = useQueryClient();
  const [editing, setEditing] = useState<ModelRouter | null>(null);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<ModelRouter | null>(null);
  const [previewing, setPreviewing] = useState<ModelRouter | null>(null);
  const [busy, setBusy] = useState(false);

  const routersQuery = useQuery({
    queryKey: ["model-routers"],
    queryFn: () => api<{ routers: ModelRouter[] }>("/admin/model-routers"),
  });
  const strategiesQuery = useQuery({
    queryKey: ["model-router-strategies"],
    queryFn: () => api<{ strategies: RouterStrategy[] }>("/admin/model-routers/strategies"),
  });

  const routers = routersQuery.data?.routers ?? [];
  const strategies = strategiesQuery.data?.strategies ?? [];

  async function remove(router: ModelRouter) {
    setBusy(true);
    try {
      await api("/admin/model-routers/" + router.id, { method: "DELETE" });
      await client.invalidateQueries({ queryKey: ["model-routers"] });
      toast(t("common.saved"), "success");
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
      setDeleting(null);
    }
  }

  async function toggle(router: ModelRouter, enabled: boolean) {
    try {
      await api("/admin/model-routers/" + router.id, {
        method: "PUT",
        body: JSON.stringify({ enabled }),
      });
      await client.invalidateQueries({ queryKey: ["model-routers"] });
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err));
    }
  }

  const columns: Column<ModelRouter>[] = [
    {
      key: "name",
      header: t("routers.name"),
      render: (r) => (
        <div>
          <div className="font-medium text-slate-800">{r.name}</div>
          {r.description ? <div className="text-xs text-slate-500">{r.description}</div> : null}
        </div>
      ),
    },
    {
      key: "strategy",
      header: t("routers.strategy"),
      render: (r) => (
        <span className="rounded bg-slate-100 px-2 py-0.5 text-xs text-slate-700">{r.strategy}</span>
      ),
    },
    {
      key: "scope",
      header: t("routers.scope"),
      render: (r) => (
        <span className="text-slate-700">
          {t(SCOPE_LABEL[r.scope])}
          {r.scope !== "global" ? <span className="ml-1 text-slate-500">#{r.scope_id}</span> : null}
        </span>
      ),
    },
    {
      key: "priority",
      header: t("routers.priority"),
      render: (r) => <span className="tabular-nums text-slate-700">{r.priority}</span>,
    },
    {
      key: "enabled",
      header: t("routers.enabled"),
      render: (r) => (
        <label className="inline-flex items-center gap-2">
          <input
            type="checkbox"
            checked={r.enabled}
            onChange={(e) => void toggle(r, e.target.checked)}
            data-testid={"router-enabled-" + r.id}
          />
          <span className={r.enabled ? "text-emerald-600" : "text-slate-400"}>
            {r.enabled ? t("common.on") : t("common.off")}
          </span>
        </label>
      ),
    },
    {
      key: "actions",
      header: "",
      className: "text-right",
      render: (r) => (
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => setPreviewing(r)}>
            {t("routers.preview")}
          </Button>
          <Button variant="ghost" onClick={() => setEditing(r)}>
            {t("common.edit")}
          </Button>
          <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(r)}>
            {t("common.delete")}
          </Button>
        </div>
      ),
    },
  ];

  if (routersQuery.isError) {
    return <QueryError error={routersQuery.error} onRetry={() => void routersQuery.refetch()} />;
  }

  const formOpen = creating || editing !== null;

  return (
    <section>
      <PageHeader
        title={t("routers.title")}
        actions={
          <Button onClick={() => setCreating(true)} disabled={strategies.length === 0}>
            {t("routers.new")}
          </Button>
        }
      />

      <p className="mb-3 text-xs text-slate-500">{t("routers.hint")}</p>

      {routersQuery.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table
          columns={columns}
          rows={routers}
          rowKey={(r) => r.id}
          empty={<EmptyState title={t("routers.empty")} description={t("routers.emptyHint")} />}
        />
      )}

      <RouterFormDrawer
        open={formOpen}
        router={editing}
        strategies={strategies}
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
        onSaved={async () => {
          await client.invalidateQueries({ queryKey: ["model-routers"] });
          setCreating(false);
          setEditing(null);
        }}
      />

      <PreviewDrawer router={previewing} onClose={() => setPreviewing(null)} />

      <ConfirmDialog
        open={deleting !== null}
        title={t("routers.deleteTitle")}
        description={t("routers.deleteMessage", { name: deleting?.name ?? "" })}
        danger
        busy={busy}
        onCancel={() => setDeleting(null)}
        onConfirm={() => (deleting ? void remove(deleting) : undefined)}
      />
    </section>
  );
}

/** RouterFormDrawer 新建或编辑一条规则；字段由策略 schema 决定。 */
function RouterFormDrawer({
  open,
  router,
  strategies,
  onClose,
  onSaved,
}: {
  open: boolean;
  router: ModelRouter | null;
  strategies: RouterStrategy[];
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const { t } = useI18n();
  const toast = useToast();
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<ModelRouterInput>(() => emptyForm(strategies));
  const [config, setConfig] = useState<Record<string, unknown>>(() => defaultConfig(strategies[0]));

  // 打开时按被编辑的规则重置表单（新建时回到默认值）。
  const [openedFor, setOpenedFor] = useState<ID | "new" | null>(null);
  const key = router?.id ?? "new";
  if (open && openedFor !== key) {
    setOpenedFor(key);
    if (router) {
      setForm({
        name: router.name,
        description: router.description,
        strategy: router.strategy,
        config: router.config,
        scope: router.scope,
        scope_id: router.scope_id,
        priority: router.priority,
        enabled: router.enabled,
      });
      setConfig(parseConfig(router.config));
    } else {
      const base = emptyForm(strategies);
      setForm(base);
      setConfig(defaultConfig(strategies.find((s) => s.name === base.strategy)));
    }
    setError(null);
  }

  const activeStrategy = strategies.find((s) => s.name === form.strategy);

  function switchStrategy(name: string) {
    const s = strategies.find((x) => x.name === name);
    setForm((f) => ({ ...f, strategy: name }));
    setConfig(defaultConfig(s));
  }

  function setField(field: RouterStrategyField, raw: unknown) {
    setConfig((c) => ({ ...c, [field.key]: raw }));
  }

  async function save() {
    setSaving(true);
    setError(null);
    try {
      const body: ModelRouterInput = {
        ...form,
        scope_id: form.scope === "global" ? 0 : Number(form.scope_id),
        config: JSON.stringify(config),
      };
      if (router) {
        await api("/admin/model-routers/" + router.id, { method: "PUT", body: JSON.stringify(body) });
      } else {
        await api("/admin/model-routers", { method: "POST", body: JSON.stringify(body) });
      }
      toast(t("common.saved"), "success");
      await onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Drawer
      open={open}
      title={router ? t("routers.editTitle") : t("routers.newTitle")}
      onClose={onClose}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={saving}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void save()} disabled={saving}>
            {t("common.save")}
          </Button>
        </div>
      }
    >
      <div className="space-y-4">
        {error ? <div className="rounded bg-red-50 px-3 py-2 text-sm text-red-700">{error}</div> : null}

        <Field label={t("routers.name")}>
          <Input value={form.name} onChange={(e) => setForm((f) => ({ ...f, name: e.target.value }))} />
        </Field>

        <Field label={t("routers.description")}>
          <Input
            value={form.description ?? ""}
            onChange={(e) => setForm((f) => ({ ...f, description: e.target.value }))}
          />
        </Field>

        <Field label={t("routers.strategy")} hint={activeStrategy?.description}>
          <Select value={form.strategy} onChange={(e) => switchStrategy(e.target.value)}>
            {strategies.map((s) => (
              <option key={s.name} value={s.name}>
                {s.title || s.name}
              </option>
            ))}
          </Select>
        </Field>

        <div className="grid grid-cols-2 gap-3">
          <Field label={t("routers.scope")}>
            <Select
              value={form.scope}
              onChange={(e) => setForm((f) => ({ ...f, scope: e.target.value as RouterScope }))}
            >
              {SCOPES.map((s) => (
                <option key={s} value={s}>
                  {t(SCOPE_LABEL[s])}
                </option>
              ))}
            </Select>
          </Field>
          <Field label={t("routers.scopeId")} hint={form.scope === "global" ? t("routers.scopeIdGlobal") : undefined}>
            <Input
              type="number"
              min={0}
              value={form.scope_id}
              disabled={form.scope === "global"}
              onChange={(e) => setForm((f) => ({ ...f, scope_id: Number(e.target.value) }))}
            />
          </Field>
        </div>

        <Field label={t("routers.priority")} hint={t("routers.priorityHint")}>
          <Input
            type="number"
            value={form.priority}
            onChange={(e) => setForm((f) => ({ ...f, priority: Number(e.target.value) }))}
          />
        </Field>

        <label className="flex items-center gap-2 text-sm text-slate-700">
          <input
            type="checkbox"
            checked={form.enabled}
            onChange={(e) => setForm((f) => ({ ...f, enabled: e.target.checked }))}
          />
          {t("routers.enabled")}
        </label>

        <div className="border-t border-slate-200 pt-4">
          <div className="mb-2 text-sm font-medium text-slate-700">{t("routers.config")}</div>
          <div className="space-y-3">
            {(activeStrategy?.fields ?? []).map((field) => (
              <StrategyFieldInput
                key={field.key}
                field={field}
                value={config[field.key]}
                onChange={(v) => setField(field, v)}
              />
            ))}
          </div>
        </div>
      </div>
    </Drawer>
  );
}

/** StrategyFieldInput 按字段类型渲染控件（当前支持 string/int/bool/enum/model）。 */
function StrategyFieldInput({
  field,
  value,
  onChange,
}: {
  field: RouterStrategyField;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  const { t } = useI18n();

  if (field.type === "bool") {
    return (
      <label className="flex items-center gap-2 text-sm text-slate-700">
        <input type="checkbox" checked={Boolean(value)} onChange={(e) => onChange(e.target.checked)} />
        <span>{field.title}</span>
      </label>
    );
  }

  if (field.type === "enum") {
    return (
      <Field label={field.title} hint={field.description}>
        <Select value={String(value ?? "")} onChange={(e) => onChange(e.target.value)}>
          {(field.enum ?? []).map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </Select>
      </Field>
    );
  }

  if (field.type === "int") {
    return (
      <Field label={field.title} hint={field.description}>
        <Input
          type="number"
          min={field.min}
          max={field.max}
          value={Number(value ?? 0)}
          onChange={(e) => onChange(Number(e.target.value))}
        />
      </Field>
    );
  }

  if (field.type === "model") {
    // model 类型在拿到产品需求前先按数字 ID 输入：输入体验的下一步是模型下拉。
    return (
      <Field label={field.title} hint={field.description}>
        <Input
          type="number"
          min={1}
          value={Number(value ?? 0)}
          onChange={(e) => onChange(Number(e.target.value))}
          placeholder={t("routers.modelIdPlaceholder")}
        />
      </Field>
    );
  }

  return (
    <Field label={field.title} hint={field.description}>
      <Input value={String(value ?? "")} onChange={(e) => onChange(e.target.value)} />
    </Field>
  );
}

/** PreviewDrawer 用一段消息预览这条规则会把请求路由到哪里。 */
function PreviewDrawer({ router, onClose }: { router: ModelRouter | null; onClose: () => void }) {
  const { t } = useI18n();
  const [keyID, setKeyID] = useState(0);
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<RouterPreview | null>(null);
  const [error, setError] = useState<string | null>(null);

  const open = router !== null;
  const [openedFor, setOpenedFor] = useState<ID | null>(null);
  if (open && openedFor !== router.id) {
    setOpenedFor(router.id);
    setResult(null);
    setError(null);
  }

  const payload = useMemo(
    () => JSON.stringify({ api_key_id: Number(keyID), request: { messages: [{ role: "user", content: message }] } }),
    [keyID, message],
  );

  async function run() {
    if (!router) return;
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      setResult(await api<RouterPreview>("/admin/model-routers/" + router.id + "/preview", {
        method: "POST",
        body: payload,
      }));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Drawer
      open={open}
      title={t("routers.previewTitle")}
      onClose={onClose}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => void run()} disabled={busy || !router}>
            {t("routers.previewRun")}
          </Button>
        </div>
      }
    >
      <div className="space-y-4">
        <p className="text-xs text-slate-500">{t("routers.previewHint")}</p>
        <Field label={t("routers.previewKey")}>
          <Input type="number" min={1} value={keyID} onChange={(e) => setKeyID(Number(e.target.value))} />
        </Field>
        <Field label={t("routers.previewMessage")}>
          <Input value={message} onChange={(e) => setMessage(e.target.value)} />
        </Field>

        {error ? <div className="rounded bg-red-50 px-3 py-2 text-sm text-red-700">{error}</div> : null}
        {busy ? <Spinner label={t("common.loading")} /> : null}

        {result ? (
          <div className="rounded-lg border border-slate-200 bg-slate-50 p-3 text-sm" data-testid="router-preview-result">
            <div className="text-slate-500">{t("routers.previewResult")}</div>
            <div className="mt-1 font-medium text-slate-800">{result.model_name || result.model_id}</div>
            <div className="mt-2 flex flex-wrap gap-2 text-xs">
              <span className="rounded bg-slate-200 px-2 py-0.5">{result.strategy}</span>
              <span className="rounded bg-slate-200 px-2 py-0.5">{result.cause}</span>
              {result.fallback ? (
                <span className="rounded bg-amber-200 px-2 py-0.5 text-amber-900">{t("routers.previewFallback")}</span>
              ) : null}
            </div>
          </div>
        ) : null}
      </div>
    </Drawer>
  );
}

// parseConfig 容错解析策略配置：坏 JSON 时回落到空对象，避免整页崩掉。
function parseConfig(raw: string): Record<string, unknown> {
  try {
    const v = JSON.parse(raw);
    return typeof v === "object" && v !== null ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}