import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import type { AIGateModel, ApiKey, Binding, BindingInput, KeyInput, User, UserGroup } from "@/api/types";
import { QuotaRuleEditor } from "@/components/QuotaRuleEditor";
import { TokenDialog } from "@/components/TokenDialog";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { Table, type Column } from "@/components/ui/Table";
import { Badge, Button, EmptyState, Field, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useApiMutation } from "@/hooks/useApiMutation";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";
import { formatDateTime } from "@/lib/format";

type BindingForm = BindingInput & { api_key: string };

export function KeysPage() {
  const { t } = useI18n();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<ApiKey | null>(null);
  const [deleting, setDeleting] = useState<ApiKey | null>(null);
  const [rotating, setRotating] = useState<ApiKey | null>(null);
  const [plainToken, setPlainToken] = useState("");

  const list = useQuery({ queryKey: ["keys"], queryFn: () => api<{ keys: ApiKey[] }>("/admin/keys") });
  const users = useQuery({ queryKey: ["users"], queryFn: () => api<{ users: User[] }>("/admin/users") });
  const groups = useQuery({ queryKey: ["groups"], queryFn: () => api<{ user_groups: UserGroup[] }>("/admin/user-groups") });

  const remove = useApiMutation([["keys"]], (id: number) => api("/admin/keys/" + id, { method: "DELETE" }), {
    onSuccess: () => setDeleting(null),
  });
  const rotate = useApiMutation([["keys"]], (id: number) =>
    api<ApiKey>("/admin/keys/" + id + "/rotate", { method: "POST" }), {
    onSuccess: (data) => {
      setRotating(null);
      if (data.token) setPlainToken(data.token);
    },
  });

  const userName = new Map<number, string>();
  (users.data?.users ?? []).forEach((u) => userName.set(u.id, u.username));
  const groupName = new Map<number, string>();
  (groups.data?.user_groups ?? []).forEach((g) => groupName.set(g.id, g.name));

  const columns: Column<ApiKey>[] = [
    { key: "name", header: t("key.name"), render: (k) => <span className="font-medium">{k.name}</span> },
    { key: "prefix", header: t("key.prefix"), render: (k) => <span className="font-mono text-xs">{k.proxy_token_prefix}…</span> },
    { key: "user", header: t("key.user"), render: (k) => userName.get(k.user_id) ?? "#" + k.user_id },
    { key: "group", header: t("key.group"), render: (k) => groupName.get(k.group_id) ?? "—" },
    { key: "models", header: t("key.bindings"), render: (k) => k.bindings.map((b) => b.model_name).join(", ") || "—" },
    {
      key: "status",
      header: t("common.enabled"),
      render: (k) => (k.enabled ? <Badge tone="green">{t("common.enabled")}</Badge> : <Badge>{t("common.disabled")}</Badge>),
    },
    { key: "expires", header: t("key.expiresAt"), render: (k) => (k.expires_at ? formatDateTime(k.expires_at) : t("common.never")) },
    {
      key: "actions",
      header: t("common.actions"),
      render: (k) => (
        <div className="flex gap-1">
          <Button variant="ghost" onClick={() => setEditing(k)}>
            {t("common.edit")}
          </Button>
          <Button variant="ghost" onClick={() => setRotating(k)}>
            {t("key.rotate")}
          </Button>
          <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(k)}>
            {t("common.delete")}
          </Button>
        </div>
      ),
    },
  ];

  return (
    <section>
      <PageHeader title={t("key.title")} actions={<Button onClick={() => setCreating(true)}>{t("key.new")}</Button>} />
      {list.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table
          columns={columns}
          rows={list.data?.keys ?? []}
          rowKey={(k) => k.id}
          empty={<EmptyState title={t("key.emptyTitle")} description={t("key.emptyHint")} />}
        />
      )}

      {creating || editing ? (
        <KeyDrawer
          apiKey={editing}
          users={users.data?.users ?? []}
          groups={groups.data?.user_groups ?? []}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
          onCreated={setPlainToken}
        />
      ) : null}

      {plainToken ? <TokenDialog token={plainToken} onClose={() => setPlainToken("")} /> : null}

      <ConfirmDialog
        open={deleting !== null}
        title={t("common.deleteConfirm")}
        description={deleting?.name}
        danger
        busy={remove.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={() => deleting && remove.mutate(deleting.id)}
      />
      <ConfirmDialog
        open={rotating !== null}
        title={t("key.rotateConfirm")}
        description={rotating?.name}
        confirmLabel={t("key.rotate")}
        busy={rotate.isPending}
        onCancel={() => setRotating(null)}
        onConfirm={() => rotating && rotate.mutate(rotating.id)}
      />
    </section>
  );
}

function KeyDrawer({
  apiKey,
  users,
  groups,
  onClose,
  onCreated,
}: {
  apiKey: ApiKey | null;
  users: User[];
  groups: UserGroup[];
  onClose: () => void;
  onCreated: (token: string) => void;
}) {
  const { t } = useI18n();
  const models = useQuery({ queryKey: ["models"], queryFn: () => api<{ models: AIGateModel[] }>("/admin/models") });
  const [form, setForm] = useState<KeyInput>({
    name: apiKey?.name ?? "",
    user_id: apiKey?.user_id ?? 0,
    group_id: apiKey?.group_id ?? 0,
    max_tokens: apiKey?.max_tokens ?? 0,
    token_quota_rules: apiKey?.token_quota_rules ?? "",
    expires_at: apiKey?.expires_at ?? null,
    bindings: (apiKey?.bindings ?? []).map((b: Binding) => ({
      ai_model_id: b.ai_model_id,
      role: b.role,
      sort_order: b.sort_order,
    })),
  });

  const save = useApiMutation([["keys"], ["usage-summary"]], () => {
    const payload = {
      ...form,
      expires_at: form.expires_at ? new Date(form.expires_at).toISOString() : null,
      bindings: form.bindings.filter((b) => b.ai_model_id > 0),
    };
    return api<ApiKey>(apiKey ? "/admin/keys/" + apiKey.id : "/admin/keys", {
      method: apiKey ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
  }, {
    onSuccess: (data: ApiKey) => {
      onClose();
      if (!apiKey && data.token) onCreated(data.token);
    },
  });

  const set = <K extends keyof KeyInput>(key: K, value: KeyInput[K]) => setForm((f) => ({ ...f, [key]: value }));
  const setBinding = (index: number, patch: Partial<BindingForm>) =>
    setForm((f) => ({
      ...f,
      bindings: f.bindings.map((b, i) => (i === index ? { ...b, ...patch } : b)),
    }));

  return (
    <Drawer
      open
      title={apiKey ? t("key.edit") : t("key.new")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending || form.name.trim() === ""}>
            {t("common.save")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <Field label={t("key.name")}>
          <Input value={form.name} onChange={(e) => set("name", e.target.value)} />
        </Field>
        <Field label={t("key.user")}>
          <Select value={form.user_id} onChange={(e) => set("user_id", Number(e.target.value))}>
            <option value={0}>{t("common.select")}</option>
            {users.map((u) => (
              <option key={u.id} value={u.id}>
                {u.username}
              </option>
            ))}
          </Select>
        </Field>
        <Field label={t("key.group")}>
          <Select value={form.group_id} onChange={(e) => set("group_id", Number(e.target.value))}>
            <option value={0}>{t("common.select")}</option>
            {groups.map((g) => (
              <option key={g.id} value={g.id}>
                {g.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label={t("key.maxTokens")}>
          <Input type="number" min={0} value={form.max_tokens} onChange={(e) => set("max_tokens", Number(e.target.value))} />
        </Field>
        <Field label={t("key.expiresAt")} hint={t("common.never")}>
          <Input
            type="date"
            value={form.expires_at ? form.expires_at.slice(0, 10) : ""}
            onChange={(e) => set("expires_at", e.target.value ? e.target.value + "T00:00:00Z" : null)}
          />
        </Field>
        <div>
          <div className="mb-1 text-sm font-medium text-slate-700">{t("key.quota")}</div>
          <QuotaRuleEditor value={form.token_quota_rules ?? ""} onChange={(next) => set("token_quota_rules", next)} />
        </div>
        <div className="space-y-2">
          <div className="text-sm font-medium text-slate-700">{t("key.bindings")}</div>
          {form.bindings.map((binding, index) => (
            <div key={index} className="grid grid-cols-[1.6fr_0.8fr_auto] gap-2">
              <Select value={binding.ai_model_id} onChange={(e) => setBinding(index, { ai_model_id: Number(e.target.value) })}>
                <option value={0}>{t("common.select")}</option>
                {(models.data?.models ?? []).map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.model_name}
                  </option>
                ))}
              </Select>
              <Select value={binding.role} onChange={(e) => setBinding(index, { role: e.target.value })}>
                <option value="primary">{t("key.role.primary")}</option>
                <option value="secondary">{t("key.role.secondary")}</option>
              </Select>
              <Button
                variant="ghost"
                className="text-red-600"
                onClick={() => setForm((f) => ({ ...f, bindings: f.bindings.filter((_, i) => i !== index) }))}
              >
                ×
              </Button>
            </div>
          ))}
          <Button
            variant="secondary"
            onClick={() =>
              setForm((f) => ({
                ...f,
                bindings: [...f.bindings, { ai_model_id: 0, role: "primary", sort_order: f.bindings.length }],
              }))
            }
          >
            {t("key.addBinding")}
          </Button>
        </div>
      </div>
    </Drawer>
  );
}
