import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import type { UserGroup } from "@/api/types";
import { QuotaRuleEditor } from "@/components/QuotaRuleEditor";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { Table, type Column } from "@/components/ui/Table";
import { Button, EmptyState, Field, Input, PageHeader, Spinner } from "@/components/ui/primitives";
import { useApiMutation } from "@/hooks/useApiMutation";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";

export function GroupsPage() {
  const { t } = useI18n();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<UserGroup | null>(null);
  const [deleting, setDeleting] = useState<UserGroup | null>(null);

  const list = useQuery({ queryKey: ["groups"], queryFn: () => api<{ user_groups: UserGroup[] }>("/admin/user-groups") });
  const remove = useApiMutation([["groups"], ["users"]], (id: number) => api("/admin/user-groups/" + id, { method: "DELETE" }), {
    onSuccess: () => setDeleting(null),
  });

  const columns: Column<UserGroup>[] = [
    { key: "name", header: t("group.name"), render: (g) => <span className="font-medium">{g.name}</span> },
    { key: "quota", header: t("group.quota"), render: (g) => g.token_quota_rules || "—" },
    { key: "global", header: t("group.globalQuota"), render: (g) => g.global_token_quota_rules || "—" },
    {
      key: "actions",
      header: t("common.actions"),
      render: (g) => (
        <div className="flex gap-1">
          <Button variant="ghost" onClick={() => setEditing(g)}>
            {t("common.edit")}
          </Button>
          <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(g)}>
            {t("common.delete")}
          </Button>
        </div>
      ),
    },
  ];

  return (
    <section>
      <PageHeader title={t("group.title")} actions={<Button onClick={() => setCreating(true)}>{t("group.new")}</Button>} />
      {list.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table
          columns={columns}
          rows={list.data?.user_groups ?? []}
          rowKey={(g) => g.id}
          empty={<EmptyState title={t("group.emptyTitle")} />}
        />
      )}

      {creating || editing ? (
        <GroupDrawer
          group={editing}
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

function GroupDrawer({ group, onClose }: { group: UserGroup | null; onClose: () => void }) {
  const { t } = useI18n();
  const [name, setName] = useState(group?.name ?? "");
  const [quota, setQuota] = useState(group?.token_quota_rules ?? "");
  const [globalQuota, setGlobalQuota] = useState(group?.global_token_quota_rules ?? "");

  const save = useApiMutation([["groups"]], () =>
    api(group ? "/admin/user-groups/" + group.id : "/admin/user-groups", {
      method: group ? "PUT" : "POST",
      body: JSON.stringify({ name, token_quota_rules: quota, global_token_quota_rules: globalQuota }),
    }), { onSuccess: onClose });

  return (
    <Drawer
      open
      title={group ? t("group.edit") : t("group.new")}
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
        <Field label={t("group.name")}>
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <div>
          <div className="mb-1 text-sm font-medium text-slate-700">{t("group.quota")}</div>
          <QuotaRuleEditor value={quota} onChange={setQuota} />
        </div>
        <div>
          <div className="mb-1 text-sm font-medium text-slate-700">{t("group.globalQuota")}</div>
          <QuotaRuleEditor value={globalQuota} onChange={setGlobalQuota} />
        </div>
      </div>
    </Drawer>
  );
}
