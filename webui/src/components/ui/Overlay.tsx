import { type ReactNode } from "react";

import { useI18n } from "@/i18n";

import { Button } from "./primitives";
import { useModalA11y } from "./useModalA11y";

/** Drawer 是从右侧滑出的表单容器。 */
export function Drawer({
  open,
  title,
  onClose,
  children,
  footer,
}: {
  open: boolean;
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
}) {
  const { t } = useI18n();
  const panelRef = useModalA11y<HTMLDivElement>(open, onClose);

  if (!open) return null;
  return (
    <div className="fixed inset-0 z-40 flex justify-end bg-slate-900/30">
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        className="flex h-full w-full max-w-lg flex-col bg-white shadow-xl outline-none"
      >
        <header className="flex h-14 items-center justify-between border-b border-slate-200 px-4">
          <h2 className="text-base font-semibold">{title}</h2>
          <button type="button" aria-label={t("common.close")} className="text-slate-400 hover:text-slate-600" onClick={onClose}>
            ×
          </button>
        </header>
        <div className="flex-1 overflow-y-auto p-4">{children}</div>
        {footer ? <footer className="flex justify-end gap-2 border-t border-slate-200 p-4">{footer}</footer> : null}
      </div>
    </div>
  );
}

/** ConfirmDialog 用于删除、rotate 这类二次确认。 */
export function ConfirmDialog({
  open,
  title,
  description,
  confirmLabel,
  danger,
  busy,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  description?: string;
  confirmLabel?: string;
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const { t } = useI18n();
  const panelRef = useModalA11y<HTMLDivElement>(open, onCancel);
  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-900/30">
      <div
        ref={panelRef}
        role="alertdialog"
        aria-modal="true"
        aria-label={title}
        tabIndex={-1}
        className="w-full max-w-sm rounded-xl bg-white p-5 shadow-xl outline-none"
      >
        <h2 className="text-base font-semibold">{title}</h2>
        {description ? <p className="mt-2 text-sm text-slate-600">{description}</p> : null}
        <div className="mt-5 flex justify-end gap-2">
          <Button variant="secondary" onClick={onCancel}>
            {t("common.cancel")}
          </Button>
          <Button variant={danger ? "danger" : "primary"} onClick={onConfirm} disabled={busy}>
            {confirmLabel ?? t("common.confirm")}
          </Button>
        </div>
      </div>
    </div>
  );
}
