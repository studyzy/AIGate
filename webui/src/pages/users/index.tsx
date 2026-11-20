import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import type { User, UserGroup } from "@/api/types";
import { TokenDialog } from "@/components/TokenDialog";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { Table, type Column } from "@/components/ui/Table";
import { QueryError } from "@/components/ui/QueryError";
import { Badge, Button, EmptyState, Field, Input, PageHeader, Select, Spinner } from "@/components/ui/primitives";
import { useApiMutation } from "@/hooks/useApiMutation";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";

export function UsersPage() {
  const { t } = useI18n();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<User | null>(null);
  const [deleting, setDeleting] = useState<User | null>(null);
  const [rotating, setRotating] = useState<User | null>(null);
  const [plainToken, setPlainToken] = useState("");

  const list = useQuery({ queryKey: ["users"], queryFn: () => api<{ users: User[] }>("/admin/users") });
  const groups = useQuery({ queryKey: ["groups"], queryFn: () => api<{ user_groups: UserGroup[] }>("/admin/user-groups") });
  const groupName = new Map<number, string>();
  (groups.data?.user_groups ?? []).forEach((g) => groupName.set(g.id, g.name));

  const remove = useApiMutation([["users"]], (id: number) => api("/admin/users/" + id, { method: "DELETE" }), {
    onSuccess: () => setDeleting(null),
  });
  const rotate = useApiMutation([["users"]], (id: number) => api<User>("/admin/users/" + id + "/rotate", { method: "POST" }), {
    onSuccess: (data) => {
      setRotating(null);
      if (data.token) setPlainToken(data.token);
    },
  });

  const columns: Column<User>[] = [
    { key: "username", header: t("user.username"), render: (u) => <span className="font-medium">{u.username}</span> },
    { key: "role", header: t("user.role"), render: (u) => t(("user.role." + u.role) as "user.role.admin") },
    { key: "group", header: t("user.group"), render: (u) => groupName.get(u.group_id) ?? "—" },
    { key: "prefix", header: t("user.prefix"), render: (u) => <span className="font-mono text-xs">{u.api_token_prefix}…</span> },
    {
      key: "status",
      header: t("common.enabled"),
      render: (u) => (u.enabled ? <Badge tone="green">{t("common.enabled")}</Badge> : <Badge>{t("common.disabled")}</Badge>),
    },
    {
      key: "actions",
      header: t("common.actions"),
      render: (u) => (
        <div className="flex gap-1">
          <Button variant="ghost" onClick={() => setEditing(u)}>
            {t("common.edit")}
          </Button>
          <Button variant="ghost" onClick={() => setRotating(u)}>
            {t("user.rotate")}
          </Button>
          <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(u)}>
            {t("common.delete")}
          </Button>
        </div>
      ),
    },
  ];

  return (
    <section>
      <PageHeader title={t("user.title")} actions={<Button onClick={() => setCreating(true)}>{t("user.new")}</Button>} />
      {list.isError ? (
        <QueryError error={list.error} onRetry={() => void list.refetch()} />
      ) : list.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table
          columns={columns}
          rows={list.data?.users ?? []}
          rowKey={(u) => u.id}
          empty={<EmptyState title={t("user.emptyTitle")} />}
        />
      )}

      {creating || editing ? (
        <UserDrawer
          user={editing}
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
        description={deleting?.username}
        danger
        busy={remove.isPending}
        onCancel={() => setDeleting(null)}
        onConfirm={() => deleting && remove.mutate(deleting.id)}
      />
      <ConfirmDialog
        open={rotating !== null}
        title={t("user.rotateConfirm")}
        description={rotating?.username}
        confirmLabel={t("user.rotate")}
        busy={rotate.isPending}
        onCancel={() => setRotating(null)}
        onConfirm={() => rotating && rotate.mutate(rotating.id)}
      />
    </section>
  );
}

function UserDrawer({
  user,
  groups,
  onClose,
  onCreated,
}: {
  user: User | null;
  groups: UserGroup[];
  onClose: () => void;
  onCreated: (token: string) => void;
}) {
  const { t } = useI18n();
  const [username, setUsername] = useState(user?.username ?? "");
  const [role, setRole] = useState(user?.role ?? "user");
  const [groupId, setGroupId] = useState(user?.group_id ?? 0);
  const [enabled, setEnabled] = useState(user?.enabled ?? true);

  const save = useApiMutation([["users"]], () =>
    api<User>(user ? "/admin/users/" + user.id : "/admin/users", {
      method: user ? "PUT" : "POST",
      body: JSON.stringify({ username, role, group_id: groupId, enabled }),
    }), {
    onSuccess: (data) => {
      onClose();
      if (!user && data.token) onCreated(data.token);
    },
  });

  return (
    <Drawer
      open
      title={user ? t("user.edit") : t("user.new")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending || username.trim() === ""}>
            {t("common.save")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <Field label={t("user.username")}>
          <Input value={username} onChange={(e) => setUsername(e.target.value)} disabled={!!user} />
        </Field>
        <Field label={t("user.role")}>
          <Select value={role} onChange={(e) => setRole(e.target.value)}>
            <option value="user">{t("user.role.user")}</option>
            <option value="admin">{t("user.role.admin")}</option>
          </Select>
        </Field>
        <Field label={t("user.group")}>
          <Select value={groupId} onChange={(e) => setGroupId(Number(e.target.value))}>
            <option value={0}>{t("common.select")}</option>
            {groups.map((g) => (
              <option key={g.id} value={g.id}>
                {g.name}
              </option>
            ))}
          </Select>
        </Field>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          {t("common.enabled")}
        </label>
      </div>
    </Drawer>
  );
}
