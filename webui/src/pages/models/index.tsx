import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { z } from "zod";

import type { AIGateModel, ConnectivityResult, Credential } from "@/api/types";
import { Table, type Column } from "@/components/ui/Table";
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

export function ModelsPage() {
  const { t } = useI18n();
  const toast = useToast();
  const [editing, setEditing] = useState<AIGateModel | null>(null);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<AIGateModel | null>(null);

  const models = useQuery({ queryKey: ["models"], queryFn: () => api<{ models: AIGateModel[] }>("/admin/models") });
  const credentials = useQuery({
    queryKey: ["credentials"],
    queryFn: () => api<{ credentials: Credential[] }>("/admin/credentials"),
  });

  const remove = useApiMutation([["models"]], (id: number) => api("/admin/models/" + id, { method: "DELETE" }), {
    onSuccess: () => setDeleting(null),
  });

  const test = useApiMutation([], (id: number) =>
    api<ConnectivityResult>("/admin/models/" + id + "/connectivity", { method: "POST" }),
  );

  const modelNames = new Map<number, string>();
  (models.data?.models ?? []).forEach((m) => modelNames.set(m.id, m.model_name || m.model_id));

  const columns: Column<AIGateModel>[] = [
    { key: "name", header: t("model.name"), render: (m) => <span className="font-medium">{m.model_name}</span> },
    { key: "provider", header: t("model.provider"), render: (m) => m.provider },
    { key: "model", header: t("model.modelId"), render: (m) => <span className="font-mono text-xs">{m.model_id}</span> },
    { key: "type", header: t("model.type"), render: (m) => m.model_type },
    {
      key: "status",
      header: t("common.enabled"),
      render: (m) =>
        m.enabled ? <Badge tone="green">{t("common.enabled")}</Badge> : <Badge>{t("common.disabled")}</Badge>,
    },
    {
      key: "actions",
      header: t("common.actions"),
      render: (m) => (
        <div className="flex gap-2">
          <Button
            variant="ghost"
            onClick={() =>
              test.mutate(m.id, {
                onSuccess: (res) =>
                  res.ok
                    ? toast(t("model.testOk", { ms: res.latency_ms ?? 0 }), "success")
                    : toast(res.error ?? t("usage.status")),
              })
            }
          >
            {t("model.test")}
          </Button>
          <Button variant="ghost" onClick={() => setEditing(m)}>
            {t("common.edit")}
          </Button>
          <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(m)}>
            {t("common.delete")}
          </Button>
        </div>
      ),
    },
  ];

  return (
    <section>
      <PageHeader
        title={t("model.title")}
        actions={
          <Button
            onClick={() => setCreating(true)}
            disabled={(credentials.data?.credentials.length ?? 0) === 0}
            title={(credentials.data?.credentials.length ?? 0) === 0 ? t("model.emptyHint") : undefined}
          >
            {t("model.new")}
          </Button>
        }
      />
      {models.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table
          columns={columns}
          rows={models.data?.models ?? []}
          rowKey={(m) => m.id}
          empty={<EmptyState title={t("model.emptyTitle")} description={t("model.emptyHint")} />}
        />
      )}

      {creating || editing ? (
        <ModelDrawer
          model={editing}
          credentials={credentials.data?.credentials ?? []}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
          credentialNames={modelNames}
        />
      ) : null}

      <ConfirmDialog
        open={deleting !== null}
        title={t("common.delete")}
        description={deleting ? deleting.model_name : undefined}
        danger
        busy={remove.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={() => deleting && remove.mutate(deleting.id)}
      />
    </section>
  );
}

function ModelDrawer({
  model,
  credentials,
  onClose,
}: {
  model: AIGateModel | null;
  credentials: Credential[];
  credentialNames: Map<number, string>;
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
      : emptyForm,
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
    const parsed = schema.safeParse(form);
    if (!parsed.success) {
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
      title={model ? t("model.edit") : t("model.new")}
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
          <Input value={form.provider} onChange={(e) => set("provider", e.target.value)} />
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
