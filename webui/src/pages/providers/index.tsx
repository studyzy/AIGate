import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import type { AIGateModel, ConnectivityResult, Credential, SecretItem } from "@/api/types";
import { PriceEditor } from "@/components/PriceEditor";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { QueryError } from "@/components/ui/QueryError";
import { useToast } from "@/components/ui/Toast";
import { Badge, Button, EmptyState, ErrorText, Field, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";
import { microsToInput } from "@/lib/micros";

type ProviderGroup = { key: string; models: AIGateModel[] };

// CURRENCY_OPTIONS 是计价/结算币种下拉的常见选项；下拉只是便利，服务端只校验三位大写。
const CURRENCY_OPTIONS = ["CNY", "USD", "HKD", "EUR", "GBP", "JPY", "SGD", "AUD", "CAD", "KRW"];

// MAX_AUTO_SELECT 是「拉取模型」后默认勾选的上限：超过就一个都不勾，避免一次点出上百个请求。
const MAX_AUTO_SELECT = 20;

/** runBatch 串行执行并收集失败数：批量操作要能告诉调用方「几项没成功」，而不是静默半途而废。 */
async function runBatch<T>(items: T[], fn: (item: T) => Promise<unknown>): Promise<number> {
  let failed = 0;
  for (const item of items) {
    try {
      await fn(item);
    } catch {
      failed += 1;
    }
  }
  return failed;
}
type DiscoverResult = { ok: boolean; models?: string[]; error_kind?: string; message?: string };

/** 厂商只是模型上的字符串字段，分组全部在前端按 provider 归并。 */
function groupByProvider(models: AIGateModel[]): ProviderGroup[] {
  const groups = new Map<string, AIGateModel[]>();
  for (const model of models) {
    const key = model.provider.trim();
    groups.set(key, [...(groups.get(key) ?? []), model]);
  }
  return [...groups.entries()].map(([key, list]) => ({ key, models: list })).sort((a, b) => a.key.localeCompare(b.key));
}

function providerInitials(name: string): string {
  const known: Record<string, string> = {
    openai: "OA", anthropic: "AN", google: "GO", gemini: "GE", azure: "AZ",
    deepseek: "DS", moonshot: "MS", qwen: "QW", zhipu: "ZP", baidu: "BD",
  };
  return known[name.toLowerCase()] ?? (name.slice(0, 2).toUpperCase() || "??");
}

export function ProvidersPage() {
  const { t } = useI18n();
  const toast = useToast();
  const client = useQueryClient();
  const [query, setQuery] = useState("");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [editing, setEditing] = useState<ProviderGroup | null>(null);
  const [creating, setCreating] = useState(false);
  const [editingModel, setEditingModel] = useState<AIGateModel | null>(null);
  const [pricingModel, setPricingModel] = useState<AIGateModel | null>(null);
  const [deletingModel, setDeletingModel] = useState<AIGateModel | null>(null);
  const [deletingProvider, setDeletingProvider] = useState<ProviderGroup | null>(null);

  const models = useQuery({ queryKey: ["models"], queryFn: () => api<{ models: AIGateModel[] }>("/admin/models") });
  const credentials = useQuery({
    queryKey: ["credentials"],
    queryFn: () => api<{ credentials: Credential[] }>("/admin/credentials"),
  });

  const credentialName = useMemo(() => {
    const map = new Map<number, string>();
    (credentials.data?.credentials ?? []).forEach((c) => map.set(c.id, c.name));
    return map;
  }, [credentials.data]);

  const groups = useMemo(() => groupByProvider(models.data?.models ?? []), [models.data]);
  const needle = query.trim().toLowerCase();
  const visible = groups.filter(
    (g) =>
      needle === "" ||
      g.key.toLowerCase().includes(needle) ||
      g.models.some((m) => m.model_id.toLowerCase().includes(needle) || m.model_name.toLowerCase().includes(needle)),
  );

  const toggleExpanded = (key: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  async function deleteModels(ids: number[]) {
    const failed = await runBatch(ids, (id) => api("/admin/models/" + id, { method: "DELETE" }));
    await client.invalidateQueries({ queryKey: ["models"] });
    setDeletingModel(null);
    setDeletingProvider(null);
    if (failed > 0) toast(t("provider.batchFailed", { failed, total: ids.length }));
  }

  async function setEnabledAll(ids: number[], enabled: boolean) {
    const failed = await runBatch(ids, (id) =>
      api("/admin/models/" + id, { method: "PUT", body: JSON.stringify({ enabled }) }),
    );
    await client.invalidateQueries({ queryKey: ["models"] });
    if (failed > 0) toast(t("provider.batchFailed", { failed, total: ids.length }));
  }

  async function test(id: number) {
    try {
      const res = await api<ConnectivityResult>("/admin/models/" + id + "/connectivity", { method: "POST" });
      if (res.ok) {
        const parts = [t("model.testOk", { ms: res.latency_ms ?? 0 })];
        if (res.reply) parts.push(t("model.testReply", { reply: res.reply }));
        if (res.upstream_id) parts.push(res.upstream_model ? res.upstream_model + " · " + res.upstream_id : res.upstream_id);
        toast(parts.join(" · "), "success");
      } else {
        toast(res.message ?? res.error_kind ?? t("usage.status"));
      }
    } catch (err) {
      toast(err instanceof Error ? err.message : String(err));
    }
  }

  const loading = models.isLoading || credentials.isLoading;

  return (
    <section>
      <PageHeader
        title={t("provider.title")}
        actions={
          <>
            <span className="w-64">
              <Input placeholder={t("provider.search")} value={query} onChange={(e) => setQuery(e.target.value)} />
            </span>
            <Button onClick={() => setCreating(true)}>{t("provider.new")}</Button>
          </>
        }
      />

      {models.isError || credentials.isError ? (
        <QueryError
          error={models.error ?? credentials.error}
          onRetry={() => {
            void models.refetch();
            void credentials.refetch();
          }}
        />
      ) : loading ? (
        <Spinner label={t("common.loading")} />
      ) : visible.length === 0 ? (
        <EmptyState title={t("provider.empty")} description={t("provider.emptyHint")} />
      ) : (
        <div className="overflow-x-auto rounded-xl border border-slate-200 bg-white">
          <div className="grid min-w-[840px] grid-cols-[minmax(220px,1.2fr)_80px_minmax(160px,1fr)_minmax(200px,1fr)_160px] items-center gap-3 border-b border-slate-200 bg-slate-50 px-4 py-2 text-xs font-medium uppercase tracking-wide text-slate-500">
            <div>{t("provider.name")}</div>
            <div>{t("provider.models")}</div>
            <div>{t("provider.credential")}</div>
            <div>{t("provider.endpoint")}</div>
            <div className="text-right">{t("common.actions")}</div>
          </div>

          {visible.map((group) => {
            const isOpen = expanded.has(group.key);
            const ids = group.models.map((m) => m.id);
            const allEnabled = group.models.every((m) => m.enabled);
            const credentialNames = [...new Set(group.models.map((m) => credentialName.get(m.credential_id) ?? "").filter(Boolean))];
            const endpoints = [...new Set(group.models.map((m) => m.url).filter(Boolean))];
            return (
              <div key={group.key} className="border-b border-slate-100 last:border-b-0">
                <div className="grid min-w-[840px] grid-cols-[minmax(220px,1.2fr)_80px_minmax(160px,1fr)_minmax(200px,1fr)_160px] items-center gap-3 px-4 py-2.5 hover:bg-slate-50">
                  <button
                    type="button"
                    aria-expanded={isOpen}
                    onClick={() => toggleExpanded(group.key)}
                    className="flex min-w-0 items-center gap-3 rounded-md text-left focus-visible:outline focus-visible:outline-2 focus-visible:outline-blue-500"
                  >
                    <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-slate-900 text-[11px] font-semibold text-white">
                      {providerInitials(group.key || "?")}
                    </span>
                    <span className="min-w-0">
                      <span className="block truncate text-sm font-medium">{group.key || t("provider.unnamed")}</span>
                      <span className="block truncate text-xs text-slate-500">
                        {[...new Set(group.models.map((m) => m.model_type))].join(", ")}
                      </span>
                    </span>
                  </button>
                  <div><Badge>{group.models.length}</Badge></div>
                  <div className="truncate text-xs text-slate-600">{credentialNames.join(", ") || "—"}</div>
                  <div className="truncate font-mono text-xs text-slate-500" title={endpoints.join("\n")}>
                    {endpoints[0] ?? "—"}
                    {endpoints.length > 1 ? " +" + (endpoints.length - 1) : ""}
                  </div>
                  <div className="flex items-center justify-end gap-1">
                    <button
                      type="button"
                      className="rounded-md px-2 py-1 text-xs text-slate-600 hover:bg-slate-100"
                      onClick={() => void setEnabledAll(ids, !allEnabled)}
                    >
                      {allEnabled ? t("common.disabled") : t("common.enabled")}
                    </button>
                    <Button variant="ghost" onClick={() => setEditing(group)}>{t("common.edit")}</Button>
                    <Button variant="ghost" className="text-red-600" onClick={() => setDeletingProvider(group)}>
                      {t("common.delete")}
                    </Button>
                  </div>
                </div>

                {isOpen ? (
                  <div className="border-t border-slate-100 bg-slate-50/60 px-4 py-3">
                    <div className="space-y-1">
                      {group.models.map((model) => (
                        <div
                          key={model.id}
                          className="grid min-w-[760px] grid-cols-[minmax(180px,1.3fr)_minmax(110px,1fr)_70px_minmax(150px,1fr)_minmax(110px,0.9fr)_minmax(120px,0.8fr)_250px] items-center gap-2 rounded-lg bg-white px-3 py-2 text-sm"
                        >
                          <div className="min-w-0">
                            <div className="truncate font-medium">{model.model_name}</div>
                            <div className="truncate font-mono text-xs text-slate-500">{model.model_id}</div>
                          </div>
                          <div className="truncate text-xs text-slate-600">{credentialName.get(model.credential_id) ?? "—"}</div>
                          <div>
                            {model.enabled ? <Badge tone="green">{t("common.enabled")}</Badge> : <Badge>{t("common.disabled")}</Badge>}
                          </div>
                          <div className="text-xs">
                            {model.current_price ? (
                              <Badge tone="green">{model.currency}</Badge>
                            ) : model.currency ? (
                              <span className="text-amber-600">{t("model.noPrice")}</span>
                            ) : (
                              <span className="text-slate-400">{t("model.noCurrency")}</span>
                            )}
                          </div>
                          <div className="truncate font-mono text-xs text-slate-500" title={model.url}>{model.url}</div>
                          <div className="flex justify-end gap-1">
                            <Button variant="ghost" onClick={() => void test(model.id)}>{t("model.test")}</Button>
                            <Button variant="ghost" onClick={() => setPricingModel(model)}>{t("price.title")}</Button>
                            <Button variant="ghost" onClick={() => setEditingModel(model)}>{t("common.edit")}</Button>
                            <Button variant="ghost" className="text-red-600" onClick={() => setDeletingModel(model)}>
                              {t("common.delete")}
                            </Button>
                          </div>
                        </div>
                      ))}
                    </div>
                  </div>
                ) : null}
              </div>
            );
          })}
        </div>
      )}

      {creating || editing ? (
        <ProviderDrawer
          group={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      ) : null}

      {editingModel ? <ModelDrawer model={editingModel} onClose={() => setEditingModel(null)} /> : null}

      {pricingModel ? (
        <PriceEditor
          model={pricingModel}
          onClose={() => {
            setPricingModel(null);
            void client.invalidateQueries({ queryKey: ["models"] });
          }}
        />
      ) : null}

      <ConfirmDialog
        open={deletingModel !== null}
        title={t("common.delete")}
        description={deletingModel ? deletingModel.model_name : undefined}
        danger
        onCancel={() => setDeletingModel(null)}
        onConfirm={() => deletingModel && void deleteModels([deletingModel.id])}
      />
      <ConfirmDialog
        open={deletingProvider !== null}
        title={t("provider.deleteConfirm", { count: deletingProvider?.models.length ?? 0 })}
        description={deletingProvider?.key}
        danger
        onCancel={() => setDeletingProvider(null)}
        onConfirm={() => deletingProvider && void deleteModels(deletingProvider.models.map((m) => m.id))}
      />
    </section>
  );
}

function ProviderDrawer({ group, onClose }: { group: ProviderGroup | null; onClose: () => void }) {
  const { t } = useI18n();
  const toast = useToast();
  const client = useQueryClient();

  const first = group?.models[0];
  const credentialID = useMemo(() => {
    const ids = new Set((group?.models ?? []).map((m) => m.credential_id).filter((id) => id > 0));
    return ids.size === 1 ? [...ids][0] : 0;
  }, [group]);

  const credential = useQuery({
    queryKey: ["credential", credentialID],
    queryFn: () => api<Credential>("/admin/credentials/" + credentialID),
    enabled: credentialID > 0,
  });

  const [name, setName] = useState(group?.key ?? "");
  const [url, setUrl] = useState(first?.url ?? "");
  const [modelType, setModelType] = useState(first?.model_type || "openai-completions");
  // url/type 只有被真正编辑过才回写：同组模型可能有不同端点，改名不该把它们压平。
  const [urlDirty, setUrlDirty] = useState(false);
  const [typeDirty, setTypeDirty] = useState(false);
  const [credentialName, setCredentialName] = useState("");
  const [keys, setKeys] = useState<SecretItem[]>([{ key: "api_key", value: "", location: "header" }]);
  const [fetched, setFetched] = useState<string[] | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [manual, setManual] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [hydrated, setHydrated] = useState(false);

  // 编辑已有供应商时用服务端返回的掩码值回填，未改动即原样回传（服务端保留原密钥）。
  if (!hydrated && credential.data) {
    setCredentialName(credential.data.name);
    setKeys(
      credential.data.secrets.length > 0
        ? credential.data.secrets.map((s) => ({ key: s.key, value: s.value, location: s.location }))
        : [{ key: "api_key", value: "", location: "header" }],
    );
    setHydrated(true);
  }

  const existingIDs = new Set((group?.models ?? []).map((m) => m.model_id));
  const candidateIDs = (fetched ?? []).filter((id) => !existingIDs.has(id));
  const distinctURLCount = new Set((group?.models ?? []).map((m) => m.url)).size;

  async function fetchModels() {
    setError("");
    setBusy(true);
    try {
      const plain = keys.map((k) => k.value).find((v) => v && !v.includes("*"));
      const res = await api<DiscoverResult>("/admin/models/discover", {
        method: "POST",
        body: JSON.stringify({
          model_type: modelType,
          url,
          api_key: plain ?? "",
          credential_id: plain ? 0 : credentialID,
        }),
      });
      if (!res.ok) {
        setError(res.message ?? t("provider.fetchFailed"));
        return;
      }
      const list = res.models ?? [];
      const candidates = list.filter((id) => !existingIDs.has(id));
      // 上游常一次返回上百个模型；超过上限就默认不勾，让用户显式选择，避免一次点击创建大量模型。
      setFetched(list);
      setSelected(new Set(candidates.length > MAX_AUTO_SELECT ? [] : candidates));
      toast(t("provider.fetched", { count: list.length }), "success");
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  async function submit() {
    if (name.trim() === "" || url.trim() === "") {
      setError(t("common.required"));
      return;
    }
    setBusy(true);
    setError("");
    try {
      const secrets = keys.filter((k) => k.value.trim() !== "");
      let targetCredential = credentialID;
      if (credentialName.trim() !== "" && secrets.length > 0) {
        if (credentialID > 0) {
          await api("/admin/credentials/" + credentialID, {
            method: "PUT",
            body: JSON.stringify({ name: credentialName, secrets }),
          });
        } else {
          const created = await api<Credential>("/admin/credentials", {
            method: "POST",
            body: JSON.stringify({ name: credentialName, secrets, enabled: true }),
          });
          targetCredential = created.id;
        }
      }

      const patch: Record<string, unknown> = { provider: name };
      if (!group || urlDirty) patch.url = url;
      if (!group || typeDirty) patch.model_type = modelType;
      if (targetCredential > 0) patch.credential_id = targetCredential;

      const failedUpdates = await runBatch(group?.models ?? [], (model) =>
        api("/admin/models/" + model.id, { method: "PUT", body: JSON.stringify(patch) }),
      );

      const toCreate = [...(fetched ?? []).filter((id) => selected.has(id)), manual.trim()].filter(
        (id) => id !== "" && !existingIDs.has(id),
      );
      const failedCreates = await runBatch(toCreate, (id) =>
        api("/admin/models", {
          method: "POST",
          body: JSON.stringify({
            provider: name,
            model_id: id,
            model_name: id,
            url,
            model_type: modelType,
            enabled: true,
            ...(targetCredential > 0 ? { credential_id: targetCredential } : {}),
          }),
        }),
      );

      await client.invalidateQueries({ queryKey: ["models"] });
      await client.invalidateQueries({ queryKey: ["credentials"] });

      const failed = failedUpdates + failedCreates;
      if (failed > 0) {
        setError(t("provider.batchFailed", { failed, total: (group?.models.length ?? 0) + toCreate.length }));
        return;
      }
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const toggle = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  return (
    <Drawer
      open
      title={group ? t("provider.edit") : t("provider.new")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>{t("common.cancel")}</Button>
          <Button onClick={() => void submit()} disabled={busy}>{t("common.save")}</Button>
        </>
      }
    >
      <div className="space-y-3">
        <Field label={t("provider.name")}>
          <Input value={name} onChange={(e) => setName(e.target.value)} data-testid="provider-input" />
        </Field>
        <Field label={t("provider.endpoint")}>
          <Input
            value={url}
            onChange={(e) => {
              setUrl(e.target.value);
              setUrlDirty(true);
            }}
            placeholder="https://api.openai.com/v1"
          />
        </Field>
        {group && distinctURLCount > 1 && !urlDirty ? (
          <p className="-mt-1 text-xs text-amber-600">{t("provider.multiEndpoint", { count: distinctURLCount })}</p>
        ) : null}
        <Field label={t("model.type")}>
          <Select
            value={modelType}
            onChange={(e) => {
              setModelType(e.target.value);
              setTypeDirty(true);
            }}
          >
            <option value="openai-completions">openai-completions</option>
            <option value="anthropic-messages">anthropic-messages</option>
            <option value="openai-responses">openai-responses</option>
            <option value="gemini-generate-content">gemini-generate-content</option>
            <option value="openai-decisions">openai-decisions</option>
            <option value="systemone">systemone</option>
          </Select>
        </Field>

        <div className="rounded-lg border border-slate-200 p-3">
          <div className="mb-2 text-sm font-medium text-slate-700">{t("provider.credential")}</div>
          <Field label={t("provider.credentialName")}>
            <Input value={credentialName} onChange={(e) => setCredentialName(e.target.value)} />
          </Field>
          <div className="mt-2 space-y-2">
            <div className="text-xs font-medium text-slate-500">{t("provider.apiKeys")}</div>
            {keys.map((item, index) => (
              <div key={index} className="flex gap-2">
                <Input
                  type="password"
                  value={item.value}
                  onChange={(e) => setKeys(keys.map((k, i) => (i === index ? { ...k, value: e.target.value } : k)))}
                  placeholder="sk-..."
                />
                <Button
                  variant="ghost"
                  className="text-red-600"
                  onClick={() => setKeys(keys.filter((_, i) => i !== index))}
                  disabled={keys.length === 1}
                >
                  ×
                </Button>
              </div>
            ))}
            <Button
              variant="secondary"
              onClick={() => setKeys([...keys, { key: "api_key_" + (keys.length + 1), value: "", location: "header" }])}
            >
              {t("provider.addKey")}
            </Button>
            <p className="text-xs text-slate-500">{t("provider.keepKeyHint")}</p>
          </div>
        </div>

        <div className="flex items-center gap-2">
          <Button variant="secondary" onClick={() => void fetchModels()} disabled={busy || url.trim() === ""}>
            {t("provider.fetch")}
          </Button>
          {fetched ? <span className="text-xs text-slate-500">{t("provider.fetched", { count: fetched.length })}</span> : null}
        </div>

        {fetched ? (
          <div className="rounded-lg border border-slate-200 p-3">
            <div className="mb-2 text-xs font-medium text-slate-500">{t("provider.modelsToAdd")}</div>
            {candidateIDs.length > MAX_AUTO_SELECT ? (
              <p className="mb-2 text-xs text-amber-600">{t("provider.tooManyHint", { count: candidateIDs.length })}</p>
            ) : null}
            <div className="max-h-56 space-y-1 overflow-y-auto">
              {fetched.map((id) => {
                const exists = existingIDs.has(id);
                return (
                  <label key={id} className="flex items-center gap-2 text-sm">
                    <input type="checkbox" checked={exists || selected.has(id)} disabled={exists} onChange={() => toggle(id)} />
                    <span className="font-mono text-xs">{id}</span>
                    {exists ? <span className="text-xs text-slate-400">{t("provider.existing")}</span> : null}
                  </label>
                );
              })}
              {fetched.length === 0 ? <p className="text-xs text-slate-500">{t("common.empty")}</p> : null}
            </div>
            <Field label={t("provider.manualAdd")}>
              <Input value={manual} onChange={(e) => setManual(e.target.value)} placeholder={t("provider.manualAdd")} />
            </Field>
          </div>
        ) : null}

        {error ? <ErrorText>{error}</ErrorText> : null}
      </div>
    </Drawer>
  );
}

/** CURRENCY_CUSTOM 是币种下拉里「手输」的哨兵值，不是合法币种。 */
const CURRENCY_CUSTOM = "__custom";

function ModelDrawer({ model, onClose }: { model: AIGateModel; onClose: () => void }) {
  const { t } = useI18n();
  const client = useQueryClient();
  const [modelName, setModelName] = useState(model.model_name);
  const [quotaDay, setQuotaDay] = useState(model.quota_day);
  const [enabled, setEnabled] = useState(model.enabled);
  const [currency, setCurrency] = useState(model.currency);
  // 已有币种不在常见选项里时，下拉切到「手输」并保留原值。
  const [customCurrency, setCustomCurrency] = useState(
    model.currency !== "" && !CURRENCY_OPTIONS.includes(model.currency),
  );
  const [peakPricing, setPeakPricing] = useState(model.enable_peak_pricing);
  const [upstreamCost, setUpstreamCost] = useState(model.use_upstream_cost);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const price = model.current_price;

  async function submit() {
    setBusy(true);
    setError("");
    try {
      await api("/admin/models/" + model.id, {
        method: "PUT",
        body: JSON.stringify({
          model_name: modelName,
          quota_day: quotaDay,
          enabled,
          currency: currency.trim().toUpperCase(),
          enable_peak_pricing: peakPricing,
          use_upstream_cost: upstreamCost,
        }),
      });
      await client.invalidateQueries({ queryKey: ["models"] });
      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Drawer
      open
      title={t("model.edit")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>{t("common.cancel")}</Button>
          <Button onClick={() => void submit()} disabled={busy}>{t("common.save")}</Button>
        </>
      }
    >
      <div className="space-y-3">
        <Field label={t("model.modelId")}><Input value={model.model_id} disabled /></Field>
        <Field label={t("model.name")}><Input value={modelName} onChange={(e) => setModelName(e.target.value)} /></Field>
        <Field label={t("model.quotaDay")}>
          <Input type="number" min={0} value={quotaDay} onChange={(e) => setQuotaDay(Number(e.target.value))} />
        </Field>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          {t("common.enabled")}
        </label>

        <div className="space-y-2 rounded-lg border border-slate-200 p-3">
          <div className="text-sm font-medium text-slate-700">{t("model.billing")}</div>
          <Field label={t("model.currency")} hint={t("model.currencyHint")}>
            <Select
              value={customCurrency ? CURRENCY_CUSTOM : currency}
              onChange={(e) => {
                if (e.target.value === CURRENCY_CUSTOM) {
                  setCustomCurrency(true);
                  return;
                }
                setCustomCurrency(false);
                setCurrency(e.target.value);
              }}
              data-testid="model-currency-select"
            >
              <option value="">{t("model.currency.none")}</option>
              {CURRENCY_OPTIONS.map((code) => (
                <option key={code} value={code}>
                  {code}
                </option>
              ))}
              <option value={CURRENCY_CUSTOM}>{t("model.currency.custom")}</option>
            </Select>
          </Field>
          {customCurrency ? (
            <Field label={t("model.currencyCode")}>
              <Input
                value={currency}
                onChange={(e) => setCurrency(e.target.value)}
                placeholder="CNY"
                maxLength={3}
                data-testid="model-currency-input"
              />
            </Field>
          ) : null}
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={peakPricing}
              onChange={(e) => setPeakPricing(e.target.checked)}
              data-testid="model-peak-pricing"
            />
            {t("model.enablePeakPricing")}
          </label>
          <p className="text-xs text-slate-500">{t("model.enablePeakPricingHint")}</p>
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={upstreamCost}
              onChange={(e) => setUpstreamCost(e.target.checked)}
              data-testid="model-upstream-cost"
            />
            {t("model.useUpstreamCost")}
          </label>
          <p className="text-xs text-slate-500">{t("model.useUpstreamCostHint")}</p>
          <div className="flex items-center gap-2 text-xs text-slate-600" data-testid="model-current-price">
            {price ? (
              <>
                <Badge tone="green">{t("model.priceConfigured")}</Badge>
                <span>{price.currency}</span>
                <span>{t("price.output")} {microsToInput(price.offpeak_output_micros)}</span>
              </>
            ) : currency.trim() === "" ? (
              <span>{t("model.noCurrency")}</span>
            ) : (
              <span>{t("model.noPrice")}</span>
            )}
          </div>
          {error ? <ErrorText>{error}</ErrorText> : null}
        </div>
      </div>
    </Drawer>
  );
}
