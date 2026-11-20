import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import type { Webhook, WebhookDelivery } from "@/api/types";
import { ConfirmDialog, Drawer } from "@/components/ui/Overlay";
import { Table, type Column } from "@/components/ui/Table";
import { QueryError } from "@/components/ui/QueryError";
import { Badge, Button, EmptyState, Field, Input, PageHeader, Spinner } from "@/components/ui/primitives";
import { useApiMutation } from "@/hooks/useApiMutation";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";
import { formatDateTime } from "@/lib/format";

const EVENT_TYPES = ["request.completed", "quota.exceeded", "key.disabled", "key.expired"];

export function WebhooksPage() {
  const { t } = useI18n();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Webhook | null>(null);
  const [deleting, setDeleting] = useState<Webhook | null>(null);
  const [deliveries, setDeliveries] = useState<Webhook | null>(null);

  const list = useQuery({ queryKey: ["webhooks"], queryFn: () => api<{ webhooks: Webhook[] }>("/admin/webhooks") });
  const remove = useApiMutation([["webhooks"]], (id: number) => api("/admin/webhooks/" + id, { method: "DELETE" }), {
    onSuccess: () => setDeleting(null),
  });

  const columns: Column<Webhook>[] = [
    { key: "name", header: t("webhook.name"), render: (w) => <span className="font-medium">{w.name}</span> },
    { key: "url", header: t("webhook.url"), render: (w) => <span className="font-mono text-xs">{w.url}</span> },
    { key: "events", header: t("webhook.events"), render: (w) => (w.event_types.length ? w.event_types.join(", ") : "all") },
    {
      key: "status",
      header: t("common.enabled"),
      render: (w) => (w.enabled ? <Badge tone="green">{t("common.enabled")}</Badge> : <Badge>{t("common.disabled")}</Badge>),
    },
    {
      key: "actions",
      header: t("common.actions"),
      render: (w) => (
        <div className="flex gap-1">
          <Button variant="ghost" onClick={() => setDeliveries(w)}>
            {t("webhook.deliveries")}
          </Button>
          <Button variant="ghost" onClick={() => setEditing(w)}>
            {t("common.edit")}
          </Button>
          <Button variant="ghost" className="text-red-600" onClick={() => setDeleting(w)}>
            {t("common.delete")}
          </Button>
        </div>
      ),
    },
  ];

  return (
    <section>
      <PageHeader title={t("webhook.title")} actions={<Button onClick={() => setCreating(true)}>{t("webhook.new")}</Button>} />
      {list.isError ? (
        <QueryError error={list.error} onRetry={() => void list.refetch()} />
      ) : list.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table
          columns={columns}
          rows={list.data?.webhooks ?? []}
          rowKey={(w) => w.id}
          empty={<EmptyState title={t("webhook.emptyTitle")} />}
        />
      )}

      {creating || editing ? (
        <WebhookDrawer
          webhook={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
        />
      ) : null}

      {deliveries ? <DeliveriesDrawer webhook={deliveries} onClose={() => setDeliveries(null)} /> : null}

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

function WebhookDrawer({ webhook, onClose }: { webhook: Webhook | null; onClose: () => void }) {
  const { t } = useI18n();
  const [name, setName] = useState(webhook?.name ?? "");
  const [url, setUrl] = useState(webhook?.url ?? "");
  const [secret, setSecret] = useState(webhook?.secret ?? "");
  const [events, setEvents] = useState<string[]>(webhook?.event_types ?? []);
  const [enabled, setEnabled] = useState(webhook?.enabled ?? true);

  const save = useApiMutation([["webhooks"]], () =>
    api(webhook ? "/admin/webhooks/" + webhook.id : "/admin/webhooks", {
      method: webhook ? "PUT" : "POST",
      body: JSON.stringify({ name, url, secret, event_types: events, enabled }),
    }), { onSuccess: onClose });

  return (
    <Drawer
      open
      title={webhook ? t("webhook.edit") : t("webhook.new")}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            {t("common.cancel")}
          </Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending || name.trim() === "" || url.trim() === ""}>
            {t("common.save")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <Field label={t("webhook.name")}>
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label={t("webhook.url")}>
          <Input value={url} onChange={(e) => setUrl(e.target.value)} />
        </Field>
        <Field label={t("webhook.secret")}>
          <Input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} />
        </Field>
        <div>
          <div className="mb-1 text-sm font-medium text-slate-700">{t("webhook.events")}</div>
          {EVENT_TYPES.map((type) => (
            <label key={type} className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={events.includes(type)}
                onChange={(e) =>
                  setEvents(e.target.checked ? [...events, type] : events.filter((item) => item !== type))
                }
              />
              {type}
            </label>
          ))}
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          {t("common.enabled")}
        </label>
      </div>
    </Drawer>
  );
}

function DeliveriesDrawer({ webhook, onClose }: { webhook: Webhook; onClose: () => void }) {
  const { t } = useI18n();
  const query = useQuery({
    queryKey: ["webhook-deliveries", webhook.id],
    queryFn: () =>
      api<{ deliveries: WebhookDelivery[]; total: number }>("/admin/webhooks/" + webhook.id + "/deliveries?page=1&page_size=50"),
  });

  const columns: Column<WebhookDelivery>[] = [
    { key: "time", header: t("usage.time"), render: (d) => formatDateTime(d.created_at) },
    { key: "event", header: t("webhook.events"), render: (d) => d.event_type },
    { key: "status", header: t("webhook.deliveryStatus"), render: (d) => d.status },
    { key: "attempts", header: t("webhook.deliveryAttempts"), render: (d) => String(d.attempts) },
    { key: "http", header: t("webhook.deliveryHttp"), render: (d) => String(d.last_status) },
    { key: "error", header: t("webhook.deliveryError"), render: (d) => d.last_error || "—" },
  ];

  return (
    <Drawer open title={t("webhook.deliveries") + " · " + webhook.name} onClose={onClose}>
      {query.isError ? (
        <QueryError error={query.error} onRetry={() => void query.refetch()} />
      ) : query.isLoading ? (
        <Spinner label={t("common.loading")} />
      ) : (
        <Table columns={columns} rows={query.data?.deliveries ?? []} rowKey={(d) => d.id} empty={<EmptyState title={t("common.empty")} />} />
      )}
    </Drawer>
  );
}
