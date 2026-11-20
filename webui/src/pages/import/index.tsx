import { useState } from "react";

import type { ImportResult } from "@/api/types";
import { Table, type Column } from "@/components/ui/Table";
import { Button, Card, EmptyState, PageHeader } from "@/components/ui/primitives";
import { useI18n } from "@/i18n";
import { api } from "@/lib/api";

interface ImportFile {
  user_groups?: unknown[];
  users?: unknown[];
  models?: unknown[];
  keys?: unknown[];
}

type Detail = ImportResult["details"][number];

export function ImportPage() {
  const { t } = useI18n();
  const [file, setFile] = useState<ImportFile | null>(null);
  const [fileName, setFileName] = useState("");
  const [error, setError] = useState("");
  const [result, setResult] = useState<ImportResult | null>(null);
  const [busy, setBusy] = useState(false);

  async function onPick(selected: File | null) {
    setResult(null);
    setError("");
    if (!selected) {
      setFile(null);
      return;
    }
    setFileName(selected.name);
    try {
      setFile(JSON.parse(await selected.text()) as ImportFile);
    } catch {
      setFile(null);
      setError(t("common.required"));
    }
  }

  async function run() {
    if (!file) return;
    setBusy(true);
    try {
      setResult(await api<ImportResult>("/admin/import", { method: "POST", body: JSON.stringify({ ...file, dry_run: false }) }));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  const columns: Column<Detail>[] = [
    { key: "kind", header: t("import.title"), render: (d) => d.kind },
    { key: "ref", header: t("webhook.name"), render: (d) => d.ref },
    { key: "action", header: t("common.actions"), render: (d) => d.action },
    { key: "message", header: t("webhook.deliveryError"), render: (d) => d.message ?? "—" },
  ];

  return (
    <section className="max-w-4xl">
      <PageHeader title={t("import.title")} actions={<Button onClick={run} disabled={!file || busy}>{t("import.run")}</Button>} />
      <p className="mb-3 text-sm text-slate-600">{t("import.hint")}</p>

      <label className="inline-flex cursor-pointer items-center gap-2 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-sm">
        <input type="file" accept="application/json,.json" className="hidden" onChange={(e) => onPick(e.target.files?.[0] ?? null)} />
        {t("import.select")}
      </label>
      {fileName ? <span className="ml-3 text-sm text-slate-600">{fileName}</span> : null}
      {error ? <p className="mt-2 text-sm text-red-600">{error}</p> : null}

      {file ? (
        <div className="mt-4 grid grid-cols-2 gap-3 md:grid-cols-4">
          <Card title={t("nav.groups")} value={String(file.user_groups?.length ?? 0)} />
          <Card title={t("nav.users")} value={String(file.users?.length ?? 0)} />
          <Card title={t("nav.models")} value={String(file.models?.length ?? 0)} />
          <Card title={t("nav.keys")} value={String(file.keys?.length ?? 0)} />
        </div>
      ) : null}

      {result ? (
        <div className="mt-6 space-y-3">
          <p className="text-sm font-medium text-slate-700">
            {t("import.summary", {
              created: result.summary.created,
              skipped: result.summary.skipped,
              errors: result.summary.errors,
            })}
          </p>
          <Table columns={columns} rows={result.details} rowKey={(d) => d.kind + d.ref} empty={<EmptyState title={t("common.empty")} />} />
        </div>
      ) : null}
    </section>
  );
}
