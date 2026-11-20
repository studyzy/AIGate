import { useState } from "react";

import { useI18n } from "@/i18n";
import { Button } from "@/components/ui/primitives";

/** TokenDialog 只在创建/rotate 后展示一次明文 token。 */
export function TokenDialog({ token, onClose }: { token: string; onClose: () => void }) {
  const { t } = useI18n();
  const [copied, setCopied] = useState(false);
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-slate-900/40">
      <div className="w-full max-w-lg rounded-xl bg-white p-5 shadow-xl">
        <h2 className="text-base font-semibold">{t("key.oneTimeTitle")}</h2>
        <p className="mt-1 text-sm text-slate-600">{t("key.oneTimeHint")}</p>
        <pre className="mt-3 overflow-x-auto rounded-lg bg-slate-900 p-3 text-xs text-emerald-300" data-testid="token-value">
          {token}
        </pre>
        <div className="mt-4 flex justify-end gap-2">
          <Button
            variant="secondary"
            onClick={() => {
              void navigator.clipboard?.writeText(token);
              setCopied(true);
            }}
          >
            {copied ? t("common.copied") : t("common.copy")}
          </Button>
          <Button onClick={onClose}>{t("common.close")}</Button>
        </div>
      </div>
    </div>
  );
}
