import { useI18n } from "@/i18n";

/** RouteFallback 是懒加载页面的占位：保持布局不跳动，仅提示正在加载。 */
export function RouteFallback() {
  const { t } = useI18n();
  return (
    <div className="flex min-h-40 items-center justify-center text-sm text-slate-500">
      {t("common.loading")}
    </div>
  );
}
