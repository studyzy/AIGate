import { useI18n } from "@/i18n";
import type { MessageKey } from "@/i18n/en";

/** Placeholder 用于本批次尚未实现的页面，避免导航落到空白。 */
export function Placeholder({ title }: { title: MessageKey }) {
  const { t } = useI18n();
  return (
    <section>
      <h1 className="text-xl font-semibold">{t(title)}</h1>
      <p className="mt-2 text-sm text-slate-500">{t("common.empty")}</p>
    </section>
  );
}
