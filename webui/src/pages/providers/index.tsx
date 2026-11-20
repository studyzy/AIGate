import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

import type { AIGateModel, ConnectivityResult, Credential, SecretItem } from "@/api/types";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { useToast } from "@/components/ui/Toast";
import { Badge, Button, EmptyState, ErrorText, Field, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";

type ProviderGroup = { key: string; models: AIGateModel[] };
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
    for (const id of ids) await api("/admin/models/" + id, { method: "DELETE" });
    await client.invalidateQueries({ queryKey: ["models"] });
    setDeletingModel(null);
    setDeletingProvider(null);
  }

  async function setEnabledAll(ids: number[], enabled: boolean) {
    for (const id of ids) await api("/admin/models/" + id, { method: "PUT", body: JSON.stringify({ enabled }) });
    await client.invalidateQueries({ queryKey: ["models"] });
  }

  async function test(id: number) {
    try {
      const res = await api<ConnectivityResult>("/admin/models/" + id + "/connectivity", { method: "POST" });
      if (res.ok) {
        toast(t("model.testOk", { ms: res.latency_ms ?? 0 }) + (res.reply ? " · " + t("model.testReply", { reply: res.reply }) : ""), "success");
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
            <Input className="w-64" placeholder={t("provider.search")} value={query} onChange={(e) => setQuery(e.target.value)} />
            <Button onClick={() => setCreating(true)}>{t("provider.new")}</Button>
          </>
        }
      />

      {loading ? (
        <Spinner label={t("common.loading")} />
      ) : visible.length === 0 ? (
        <EmptyState title={t("provider.empty")} description={t("provider.emptyHint")} />
      ) : (
        <div className="overflow-hidden rounded-xl border border-slate-200 bg-white">
          <div className="grid grid-cols-[minmax(220px,1.2fr)_80px_minmax(160px,1fr)_minmax(200px,1fr)_160px] items-center gap-3 border-b border-slate-200 bg-slate-50 px-4 py-2 text-xs font-medium uppercase tracking-wide text-slate-500">
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
                <div
                  className="grid cursor-pointer grid-cols-[minmax(220px,1.2fr)_80px_minmax(160px,1fr)_minmax(200px,1fr)_160px] items-center gap-3 px-4 py-2.5 hover:bg-slate-50"
                  onClick={() => toggleExpanded(group.key)}
                >
                  <div className="flex min-w-0 items-center gap-3">
                    <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-slate-900 text-[11px] font-semibold text-white">
                      {providerInitials(group.key || "?")}
                    </span>
                    <div className="min-w-0">
                      <div className="truncate text-sm font-medium">{group.key || t("provider.unnamed")}</div>
                      <div className="truncate text-xs text-slate-500">
                        {[...new Set(group.models.map((m) => m.model_type))].join(", ")}
                      </div>
                    </div>
                  </div>
                  <div><Badge>{group.models.length}</Badge></div>
                  <div className="truncate text-xs text-slate-600">{credentialNames.join(", ") || "—"}</div>
                  <div className="truncate font-mono text-xs text-slate-500" title={endpoints.join("\n")}>
                    {endpoints[0] ?? "—"}
                    {endpoints.length > 1 ? " +" + (endpoints.length - 1) : ""}
                  </div>
                  <div className="flex items-center justify-end gap-1" onClick={(e) => e.stopPropagation()}>
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
                          className="grid grid-cols-[minmax(200px,1.4fr)_minmax(120px,1fr)_80px_minmax(120px,0.8fr)_210px] items-center gap-2 rounded-lg bg-white px-3 py-2 text-sm"
                        >
                          <div className="min-w-0">
                            <div className="truncate font-medium">{model.model_name}</div>
                            <div className="truncate font-mono text-xs text-slate-500">{model.model_id}</div>
                          </div>
                          <div className="truncate text-xs text-slate-600">{credentialName.get(model.credential_id) ?? "—"}</div>
                          <div>
                            {model.enabled ? <Badge tone="green">{t("common.enabled")}</Badge> : <Badge>{t("common.disabled")}</Badge>}
                          </div>
                          <div className="truncate font-mono text-xs text-slate-500" title={model.url}>{model.url}</div>
                          <div className="flex justify-end gap-1">
                            <Button variant="ghost" onClick={() => void test(model.id)}>{t("model.test")}</Button>
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
      setFetched(list);
      setSelected(new Set(list.filter((id) => !existingIDs.has(id))));
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

      for (const model of group?.models ?? []) {
        await api("/admin/models/" + model.id, {
          method: "PUT",
          body: JSON.stringify({
            provider: name,
            url,
            model_type: modelType,
            ...(targetCredential > 0 ? { credential_id: targetCredential } : {}),
          }),
        });
      }

      const toCreate = [...(fetched ?? []).filter((id) => selected.has(id)), manual.trim()].filter(
        (id) => id !== "" && !existingIDs.has(id),
      );
      for (const id of toCreate) {
        await api("/admin/models", {
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
        });
      }

      await client.invalidateQueries({ queryKey: ["models"] });
      await client.invalidateQueries({ queryKey: ["credentials"] });
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
          <Input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://api.openai.com/v1" />
        </Field>
        <Field label={t("model.type")}>
          <Select value={modelType} onChange={(e) => setModelType(e.target.value)}>
            <option value="openai-completions">openai-completions</option>
            <option value="anthropic-messages">anthropic-messages</option>
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

function ModelDrawer({ model, onClose }: { model: AIGateModel; onClose: () => void }) {
  const { t } = useI18n();
  const client = useQueryClient();
  const [modelName, setModelName] = useState(model.model_name);
  const [quotaDay, setQuotaDay] = useState(model.quota_day);
  const [enabled, setEnabled] = useState(model.enabled);
  const [busy, setBusy] = useState(false);

  async function submit() {
    setBusy(true);
    try {
      await api("/admin/models/" + model.id, {
        method: "PUT",
        body: JSON.stringify({ model_name: modelName, quota_day: quotaDay, enabled }),
      });
      await client.invalidateQueries({ queryKey: ["models"] });
      onClose();
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
      </div>
    </Drawer>
  );
}
