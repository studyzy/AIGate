import { useI18n, type Lang } from "@/i18n";

export function LangSwitch() {
  const { pref, setLang, t } = useI18n();
  return (
    <label className="flex items-center gap-2 text-sm text-slate-600">
      <span className="sr-only">{t("lang.label")}</span>
      <select
        className="rounded-md border border-slate-300 bg-white px-2 py-1 text-sm"
        value={pref}
        onChange={(e) => setLang(e.target.value as Lang)}
      >
        <option value="system">{t("lang.system")}</option>
        <option value="en">{t("lang.en")}</option>
        <option value="zh-CN">{t("lang.zh-CN")}</option>
      </select>
    </label>
  );
}
