import { useRef, useState } from "react";

import { useI18n } from "@/i18n";
import { Button } from "@/components/ui/primitives";
import { useModalA11y } from "@/components/ui/useModalA11y";

/** TokenDialog 只在创建/rotate 后展示一次明文 token。 */
export function TokenDialog({ token, onClose }: { token: string; onClose: () => void }) {
  const { t } = useI18n();
  const [copied, setCopied] = useState(false);
  const preRef = useRef<HTMLPreElement>(null);
  const panelRef = useModalA11y<HTMLDivElement>(true, onClose);

  // 纯 HTTP 部署下 navigator.clipboard 不存在（非安全上下文），
  // 直接置 copied 会谎报成功；这里退回「选中 + execCommand」并以其结果为准。
  async function copy() {
    if (navigator.clipboard?.writeText) {
      try {
        await navigator.clipboard.writeText(token);
        setCopied(true);
        return;
      } catch {
        // 权限被拒时落到下面的兼容路径。
      }
    }
    const el = preRef.current;
    if (!el) return;
    const range = document.createRange();
    range.selectNodeContents(el);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    setCopied(document.execCommand("copy"));
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-900/40">
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={t("key.oneTimeTitle")}
        tabIndex={-1}
        className="w-full max-w-lg rounded-xl bg-white p-5 shadow-xl outline-none"
      >
        <h2 className="text-base font-semibold">{t("key.oneTimeTitle")}</h2>
        <p className="mt-1 text-sm text-slate-600">{t("key.oneTimeHint")}</p>
        <pre ref={preRef} className="mt-3 overflow-x-auto rounded-lg bg-slate-900 p-3 text-xs text-emerald-300" data-testid="token-value">
          {token}
        </pre>
        <div className="mt-4 flex justify-end gap-2">
          <Button variant="secondary" onClick={() => void copy()}>
            {copied ? t("common.copied") : t("common.copy")}
          </Button>
          <Button onClick={onClose}>{t("common.close")}</Button>
        </div>
      </div>
    </div>
  );
}
