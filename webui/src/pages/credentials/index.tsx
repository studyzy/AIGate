import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import type { Credential, SecretItem } from "@/api/types";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { Table, type Column } from "@/components/ui/Table";
import { Badge, Button, EmptyState, Input, PageHeader, Spinner } from "@/components/ui/primitives";
import { useApiMutation } from "@/hooks/useApiMutation";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";
import { formatDateTime } from "@/lib/format";

const emptySecret: SecretItem = { key: "", value: "", location: "" };

export function CredentialsPage() {
  const { t } = useI18n();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Credential | null>(null);
  const [deleting, setDeleting] = useState<Credential | null>(null);

  const list = useQuery({
    queryKey: ["credentials"],
    queryFn: () => api<{ credentials: Credential[] }>("/admin/credentials"),
  });
  const remove = useApiMutation([["credentials"], ["models"]], (id: number) =>
    api("/admin/credentials/" + id, { method: "DELETE" }), { onSuccess: () => setDeleting(null) });

  const columns: Column<Credential>[] = [
    { key: "name", header: t("credential.name"), render: (c) => <span className="font-medium">{c.name}</span> },
    { key: "description", header: t("credential.description"), render: (c) => c.description || "—" },
    { key: "secrets", header: t("credential.secrets"), render: (c) => String(c.secrets.length) },
    {
      key: "status",
      header: t("common.enabled"),
      render: (c) => (c.enabled ? <Badge tone="green">{t("common.enabled")}</Badge> : <Badge>{t("common.disabled")}</Badge>),
    },
    { key: "updated", header: t("usage.time"), render: (c) => formatDateTime(c.updated_at) },
    {
      key: "actions",
      header: t("common.actions"),
      render: (c) => (
        <div className="flex gap-2">
          <Button variant="ghost" onClick={() => setEditing(c)}>
            {t("common.edit")}
          </Button>
          <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(c)}>
            {t("common.delete")}
          </Button>
        </div>
      ),
    },
  ];

  return (
    <section>
      <PageHeader title={t("credential.title")} actions={<Button onClick={() => setCreating(true)}>{t("credential.new")}</Button>} />
      {list.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table
          columns={columns}
          rows={list.data?.credentials ?? []}
          rowKey={(c) => c.id}
          empty={<EmptyState title={t("credential.emptyTitle")} description={t("credential.emptyHint")} />}
        />
      )}

      {creating || editing ? (
        <CredentialDrawer
          credential={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      ) : null}

      <ConfirmDialog
        open={deleting !== null}
        title={t("common.deleteConfirm")}
        description={deleting?.name}
        danger
        busy={remove.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={() => deleting && remove.mutate(deleting.id)}
      />
    </section>
  );
}

function CredentialDrawer({ credential, onClose }: { credential: Credential | null; onClose: () => void }) {
  const { t } = useI18n();
  const [name, setName] = useState(credential?.name ?? "");
  const [description, setDescription] = useState(credential?.description ?? "");
  const [enabled, setEnabled] = useState(credential?.enabled ?? true);
  const [secrets, setSecrets] = useState<SecretItem[]>(
    credential && credential.secrets.length > 0 ? credential.secrets : [{ ...emptySecret }],
  );

  const save = useApiMutation([["credentials"]], () => {
    const body = { name, description, enabled, secrets: secrets.filter((s) => s.key.trim() !== "") };
    return api(credential ? "/admin/credentials/" + credential.id : "/admin/credentials", {
      method: credential ? "PUT" : "POST",
      body: JSON.stringify(body),
    });
  }, { onSuccess: onClose });

  function updateSecret(index: number, patch: Partial<SecretItem>) {
    setSecrets((prev) => prev.map((s, i) => (i === index ? { ...s, ...patch } : s)));
  }

  return (
    <Drawer
      open
      title={credential ? t("credential.edit") : t("credential.new")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending || name.trim() === ""}>
            {t("common.save")}
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        <label className="block">
          <span className="mb-1 block text-sm font-medium text-slate-700">{t("credential.name")}</span>
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="block">
          <span className="mb-1 block text-sm font-medium text-slate-700">{t("credential.description")}</span>
          <Input value={description} onChange={(e) => setDescription(e.target.value)} />
        </label>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          {t("common.enabled")}
        </label>

        <div className="space-y-2">
          <div className="text-sm font-medium text-slate-700">{t("credential.secrets")}</div>
          {secrets.map((secret, index) => (
            <div key={index} className="grid grid-cols-[1fr_1.4fr_auto] gap-2">
              <Input placeholder={t("credential.secretKey")} value={secret.key} onChange={(e) => updateSecret(index, { key: e.target.value })} />
              <Input placeholder={t("credential.secretValue")} value={secret.value} onChange={(e) => updateSecret(index, { value: e.target.value })} />
              <Button variant="ghost" className="text-red-600" onClick={() => setSecrets(secrets.filter((_, i) => i !== index))}>
                ×
              </Button>
            </div>
          ))}
          <Button variant="secondary" onClick={() => setSecrets([...secrets, { ...emptySecret }])}>
            {t("credential.addSecret")}
          </Button>
          <p className="text-xs text-slate-500">{t("credential.keepHint")}</p>
        </div>
      </div>
    </Drawer>
  );
}
