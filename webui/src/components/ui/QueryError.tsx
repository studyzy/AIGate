import { useI18n } from "@/i18n";

import { Button } from "./primitives";

/** QueryError 显式呈现查询失败：否则页面会把「请求失败」渲染成「暂无数据」。 */
export function QueryError({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const { t } = useI18n();
  const message = error instanceof Error && error.message ? error.message : String(error);
  return (
    <div className="rounded-xl border border-red-200 bg-red-50 p-4" role="alert">
      <p className="text-sm font-medium text-red-700">{t("common.loadFailed")}</p>
      <p className="mt-1 break-words text-sm text-red-600">{message}</p>
      {onRetry ? (
        <Button variant="secondary" className="mt-3" onClick={onRetry}>
          {t("common.retry")}
        </Button>
      ) : null}
    </div>
  );
}
