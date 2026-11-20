import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { z } from "zod";

import type { AIGateModel, ConnectivityResult, Credential } from "@/api/types";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { useToast } from "@/components/ui/Toast";
import { Badge, Button, EmptyState, ErrorText, Field, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useApiMutation } from "@/hooks/useApiMutation";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";

const schema = z.object({
  provider: z.string().trim().min(1),
  model_id: z.string().trim().min(1),
  model_name: z.string().trim().min(1),
  url: z.string().trim().min(1),
  model_type: z.string().trim().min(1),
  quota_day: z.coerce.number().int().min(0),
});

type FormState = {
  provider: string;
  model_id: string;
  model_name: string;
  url: string;
  model_type: string;
  credential_id: number;
  quota_day: number;
  enabled: boolean;
  user_key_required: boolean;
  api_key: string;
};

type ProviderGroup = { key: string; models: AIGateModel[] };

const emptyForm: FormState = {
  provider: "",
  model_id: "",
  model_name: "",
  url: "",
  model_type: "openai-completions",
  credential_id: 0,
  quota_day: 0,
  enabled: true,
  user_key_required: false,
  api_key: "",
};

/** 厂商只是模型上的字符串字段，分组全部在前端按 provider 归并。 */
function groupByProvider(models: AIGateModel[]): ProviderGroup[] {
  const groups = new Map<string, AIGateModel[]>();
  for (const model of models) {
    const key = model.provider.trim();
    groups.set(key, [...(groups.get(key) ?? []), model]);
  }
  return [...groups.entries()]
    .map(([key, list]) => ({ key, models: list }))
    .sort((a, b) => a.key.localeCompare(b.key));
}

function providerInitials(name: string): string {
  const known: Record<string, string> = {
    openai: "OA",
    anthropic: "AN",
    google: "GO",
    gemini: "GE",
    azure: "AZ",
    deepseek: "DS",
    moonshot: "MS",
    qwen: "QW",
    zhipu: "ZP",
    baidu: "BD",
  };
  const key = name.toLowerCase();
  if (known[key]) return known[key];
  return name.slice(0, 2).toUpperCase() || "??";
}

export function ProvidersPage() {
  const { t } = useI18n();
  const toast = useToast();
  const [query, setQuery] = useState("");
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [editing, setEditing] = useState<AIGateModel | null>(null);
  const [creating, setCreating] = useState<{ provider: string } | null>(null);
  const [renaming, setRenaming] = useState<ProviderGroup | null>(null);
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
      g.models.some(
        (m) =>
          m.model_id.toLowerCase().includes(needle) ||
          m.model_name.toLowerCase().includes(needle),
      ),
  );

  const toggleExpanded = (key: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const removeModel = useApiMutation([["models"]], (id: number) => api("/admin/models/" + id, { method: "DELETE" }), {
    onSuccess: () => setDeletingModel(null),
  });

  const removeProvider = useApiMutation([["models"]], async (ids: number[]) => {
    for (const id of ids) await api("/admin/models/" + id, { method: "DELETE" });
  }, { onSuccess: () => setDeletingProvider(null) });

  const setEnabledAll = useApiMutation([["models"]], async (vars: { ids: number[]; enabled: boolean }) => {
    for (const id of vars.ids) {
      await api("/admin/models/" + id, { method: "PUT", body: JSON.stringify({ enabled: vars.enabled }) });
    }
  });

  const renameProvider = useApiMutation([["models"]], async (vars: { ids: number[]; name: string }) => {
    for (const id of vars.ids) {
      await api("/admin/models/" + id, { method: "PUT", body: JSON.stringify({ provider: vars.name }) });
    }
  }, { onSuccess: () => setRenaming(null) });

  const test = useApiMutation([], (id: number) =>
    api<ConnectivityResult>("/admin/models/" + id + "/connectivity", { method: "POST" }),
  );

  const loading = models.isLoading || credentials.isLoading;

  return (
    <section>
      <PageHeader
        title={t("provider.title")}
        actions={
          <>
            <Input
              className="w-64"
              placeholder={t("provider.search")}
              value={query}
              onChange={(e) => setQuery(e.target.value)}
            />
            <Button
              onClick={() => setCreating({ provider: "" })}
              disabled={(credentials.data?.credentials.length ?? 0) === 0}
              title={(credentials.data?.credentials.length ?? 0) === 0 ? t("model.emptyHint") : undefined}
            >
              {t("provider.new")}
            </Button>
          </>
        }
      />

      {loading ? (
        <Spinner label={t("common.loading")} />
      ) : visible.length === 0 ? (
        <EmptyState title={t("provider.empty")} description={t("provider.emptyHint")} />
      ) : (
        <div className="overflow-hidden rounded-xl border border-slate-200 bg-white">
          <div className="grid grid-cols-[minmax(220px,1.2fr)_80px_minmax(160px,1fr)_minmax(200px,1fr)_150px] items-center gap-3 border-b border-slate-200 bg-slate-50 px-4 py-2 text-xs font-medium uppercase tracking-wide text-slate-500">
            <div>{t("provider.name")}</div>
            <div>{t("provider.models")}</div>
            <div>{t("provider.credentials")}</div>
            <div>{t("provider.endpoint")}</div>
            <div className="text-right">{t("common.actions")}</div>
          </div>

          {visible.map((group) => {
            const isOpen = expanded.has(group.key);
            const ids = group.models.map((m) => m.id);
            const allEnabled = group.models.every((m) => m.enabled);
            const credentialNames = [
              ...new Set(group.models.map((m) => credentialName.get(m.credential_id) ?? "").filter(Boolean)),
            ];
            const endpoints = [...new Set(group.models.map((m) => m.url).filter(Boolean))];
            return (
              <div key={group.key} className="border-b border-slate-100 last:border-b-0">
                <div
                  className="grid cursor-pointer grid-cols-[minmax(220px,1.2fr)_80px_minmax(160px,1fr)_minmax(200px,1fr)_150px] items-center gap-3 px-4 py-2.5 hover:bg-slate-50"
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
                  <div>
                    <Badge tone={group.models.length > 0 ? "slate" : "amber"}>{group.models.length}</Badge>
                  </div>
                  <div className="truncate text-xs text-slate-600">{credentialNames.join(", ") || "—"}</div>
                  <div className="truncate font-mono text-xs text-slate-500" title={endpoints.join("\n")}>
                    {endpoints[0] ?? "—"}
                    {endpoints.length > 1 ? " +" + (endpoints.length - 1) : ""}
                  </div>
                  <div className="flex items-center justify-end gap-1" onClick={(e) => e.stopPropagation()}>
                    <button
                      type="button"
                      className="rounded-md px-2 py-1 text-xs text-slate-600 hover:bg-slate-100"
                      onClick={() => setEnabledAll.mutate({ ids, enabled: !allEnabled })}
                      data-testid={"provider-toggle-" + group.key}
                    >
                      {allEnabled ? t("common.disabled") : t("common.enabled")}
                    </button>
                    <Button variant="ghost" onClick={() => setRenaming(group)}>
                      {t("common.edit")}
                    </Button>
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
                          <div className="truncate text-xs text-slate-600">
                            {credentialName.get(model.credential_id) ?? "—"}
                          </div>
                          <div>
                            {model.enabled ? (
                              <Badge tone="green">{t("common.enabled")}</Badge>
                            ) : (
                              <Badge>{t("common.disabled")}</Badge>
                            )}
                          </div>
                          <div className="truncate font-mono text-xs text-slate-500" title={model.url}>
                            {model.url}
                          </div>
                          <div className="flex justify-end gap-1">
                            <Button
                              variant="ghost"
                              onClick={() =>
                                test.mutate(model.id, {
                                  onSuccess: (res) =>
                                    res.ok
                                      ? toast(t("model.testOk", { ms: res.latency_ms ?? 0 }), "success")
                                      : toast(res.error ?? t("usage.status")),
                                })
                              }
                            >
                              {t("model.test")}
                            </Button>
                            <Button variant="ghost" onClick={() => setEditing(model)}>
                              {t("common.edit")}
                            </Button>
                            <Button variant="ghost" className="text-red-600" onClick={() => setDeletingModel(model)}>
                              {t("common.delete")}
                            </Button>
                          </div>
                        </div>
                      ))}
                    </div>
                    <div className="mt-2">
                      <Button variant="secondary" onClick={() => setCreating({ provider: group.key })}>
                        {t("provider.addModel")}
                      </Button>
                    </div>
                  </div>
                ) : null}
              </div>
            );
          })}
        </div>
      )}

      {creating || editing ? (
        <ModelDrawer
          model={editing}
          presetProvider={creating?.provider ?? ""}
          providers={groups.map((g) => g.key).filter(Boolean)}
          credentials={credentials.data?.credentials ?? []}
          onClose={() => {
            setCreating(null);
            setEditing(null);
          }}
        />
      ) : null}

      {renaming ? (
        <RenameDrawer
          group={renaming}
          onClose={() => setRenaming(null)}
          onSave={(name) => renameProvider.mutate({ ids: renaming.models.map((m) => m.id), name })}
        />
      ) : null}

      <ConfirmDialog
        open={deletingModel !== null}
        title={t("common.delete")}
        description={deletingModel ? deletingModel.model_name : undefined}
        danger
        busy={removeModel.isPending}
        onCancel={() => setDeletingModel(null)}
        onConfirm={() => deletingModel && removeModel.mutate(deletingModel.id)}
      />
      <ConfirmDialog
        open={deletingProvider !== null}
        title={t("provider.deleteConfirm", { count: deletingProvider?.models.length ?? 0 })}
        description={deletingProvider?.key}
        danger
        busy={removeProvider.isPending}
        onCancel={() => setDeletingProvider(null)}
        onConfirm={() => deletingProvider && removeProvider.mutate(deletingProvider.models.map((m) => m.id))}
      />
    </section>
  );
}

function RenameDrawer({
  group,
  onClose,
  onSave,
}: {
  group: ProviderGroup;
  onClose: () => void;
  onSave: (name: string) => void;
}) {
  const { t } = useI18n();
  const [name, setName] = useState(group.key);
  return (
    <Drawer
      open
      title={t("provider.edit")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => onSave(name.trim())} disabled={name.trim() === ""}>
            {t("common.save")}
          </Button>
        </>
      }
    >
      <Field label={t("provider.name")} hint={t("provider.renameHint", { count: group.models.length })}>
        <Input value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
    </Drawer>
  );
}

function ModelDrawer({
  model,
  presetProvider,
  providers,
  credentials,
  onClose,
}: {
  model: AIGateModel | null;
  presetProvider: string;
  providers: string[];
  credentials: Credential[];
  onClose: () => void;
}) {
  const { t } = useI18n();
  const [form, setForm] = useState<FormState>(
    model
      ? {
          provider: model.provider,
          model_id: model.model_id,
          model_name: model.model_name,
          url: model.url,
          model_type: model.model_type || "openai-completions",
          credential_id: model.credential_id,
          quota_day: model.quota_day,
          enabled: model.enabled,
          user_key_required: model.user_key_required,
          api_key: "",
        }
      : { ...emptyForm, provider: presetProvider },
  );
  const [error, setError] = useState("");

  const save = useApiMutation([["models"]], (payload: FormState) => {
    const body: Record<string, unknown> = {
      provider: payload.provider,
      model_id: payload.model_id,
      model_name: payload.model_name,
      url: payload.url,
      model_type: payload.model_type,
      credential_id: payload.credential_id,
      quota_day: payload.quota_day,
      enabled: payload.enabled,
    };
    if (payload.api_key) body.api_key = payload.api_key;
    if (!model) body.user_key_required = payload.user_key_required;
    return api(model ? "/admin/models/" + model.id : "/admin/models", {
      method: model ? "PUT" : "POST",
      body: JSON.stringify(body),
    });
  }, { onSuccess: onClose });

  function submit() {
    if (!schema.safeParse(form).success) {
      setError(t("common.required"));
      return;
    }
    setError("");
    save.mutate(form);
  }

  const set = <K extends keyof FormState>(key: K, value: FormState[K]) => setForm((f) => ({ ...f, [key]: value }));

  return (
    <Drawer
      open
      title={model ? t("model.edit") : t("provider.addModel")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={submit} disabled={save.isPending}>
            {t("common.save")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <Field label={t("model.name")}>
          <Input value={form.model_name} onChange={(e) => set("model_name", e.target.value)} />
        </Field>
        <Field label={t("model.provider")}>
          <Input
            list="provider-options"
            value={form.provider}
            onChange={(e) => set("provider", e.target.value)}
            data-testid="provider-input"
          />
          <datalist id="provider-options">
            {providers.map((name) => (
              <option key={name} value={name} />
            ))}
          </datalist>
        </Field>
        <Field label={t("model.modelId")}>
          <Input value={form.model_id} onChange={(e) => set("model_id", e.target.value)} />
        </Field>
        <Field label={t("model.url")}>
          <Input value={form.url} onChange={(e) => set("url", e.target.value)} />
        </Field>
        <Field label={t("model.type")}>
          <Select value={form.model_type} onChange={(e) => set("model_type", e.target.value)}>
            <option value="openai-completions">openai-completions</option>
            <option value="anthropic-messages">anthropic-messages</option>
          </Select>
        </Field>
        <Field label={t("model.credential")}>
          <Select value={form.credential_id} onChange={(e) => set("credential_id", Number(e.target.value))}>
            <option value={0}>{t("common.select")}</option>
            {credentials.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label={t("model.apiKey")} hint={model ? t("credential.keepHint") : undefined}>
          <Input type="password" value={form.api_key} onChange={(e) => set("api_key", e.target.value)} />
        </Field>
        <Field label={t("model.quotaDay")}>
          <Input type="number" min={0} value={form.quota_day} onChange={(e) => set("quota_day", Number(e.target.value))} />
        </Field>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={form.enabled} onChange={(e) => set("enabled", e.target.checked)} />
          {t("common.enabled")}
        </label>
        {!model ? (
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={form.user_key_required}
              onChange={(e) => set("user_key_required", e.target.checked)}
            />
            {t("model.userKeyRequired")}
          </label>
        ) : null}
        {error ? <ErrorText>{error}</ErrorText> : null}
      </div>
    </Drawer>
  );
}
