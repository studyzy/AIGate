import { useSyncExternalStore } from "react";

import { en, type MessageKey } from "./en";
import { zhCN } from "./zh-CN";

export type Lang = "system" | "en" | "zh-CN";
export type ResolvedLang = "en" | "zh-CN";

const STORAGE_KEY = "aigate.ui.lang";
const listeners = new Set<() => void>();

function readPref(): Lang {
  const raw = typeof localStorage === "undefined" ? null : localStorage.getItem(STORAGE_KEY);
  return raw === "en" || raw === "zh-CN" || raw === "system" ? raw : "system";
}

let pref: Lang = readPref();

function resolve(pref: Lang): ResolvedLang {
  if (pref !== "system") return pref;
  const lang = typeof navigator === "undefined" ? "en" : navigator.language;
  return lang.toLowerCase().startsWith("zh") ? "zh-CN" : "en";
}

function applyDocumentLang(lang: ResolvedLang) {
  if (typeof document !== "undefined") document.documentElement.lang = lang;
}

// applyDocumentLang 在首屏与切换时同步 <html lang>。
applyDocumentLang(resolve(pref));

export function getLangPref(): Lang {
  return pref;
}

export function setLang(next: Lang) {
  pref = next;
  if (typeof localStorage !== "undefined") localStorage.setItem(STORAGE_KEY, next);
  applyDocumentLang(resolve(next));
  listeners.forEach((fn) => fn());
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

const dicts: Record<ResolvedLang, Record<MessageKey, string>> = { en, "zh-CN": zhCN };

/** translate 按当前语言取文案，支持 {name} 占位符替换。 */
export function translate(lang: ResolvedLang, key: MessageKey, vars?: Record<string, string | number>): string {
  const template = dicts[lang][key] ?? en[key];
  if (!vars) return template;
  return template.replace(/\{(\w+)\}/g, (_, name: string) => String(vars[name] ?? "{" + name + "}"));
}

export function useI18n() {
  const current = useSyncExternalStore(subscribe, getLangPref, getLangPref);
  const lang = resolve(current);
  return {
    lang,
    pref: current,
    setLang,
    t: (key: MessageKey, vars?: Record<string, string | number>) => translate(lang, key, vars),
  };
}
