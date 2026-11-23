import { useEffect, useState } from "react";

import type { BuildInfo } from "@/api/types";
import { useI18n } from "@/i18n";

/** 构建信息展示在侧栏底部：版本号常显，commit/构建时间等细节走 title 悬浮提示。 */
function shortCommit(commit: string): string {
  return /^[0-9a-f]{7,}$/i.test(commit) ? commit.slice(0, 7) : commit;
}

export function BuildInfoBadge() {
  const { t } = useI18n();
  const [info, setInfo] = useState<BuildInfo | null>(null);

  // 仅取一次：构建信息随二进制固定，不轮询。失败时静默隐藏，不影响侧栏。
  useEffect(() => {
    let cancelled = false;
    fetch("/version")
      .then((res) => (res.ok ? (res.json() as Promise<BuildInfo>) : null))
      .then((body) => {
        if (!cancelled && body) setInfo(body);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, []);

  if (!info) return null;

  const unknown = t("version.unknown");
  const value = (raw: string) => (raw && raw !== "unknown" ? raw : unknown);
  const tooltip = [
    `${t("version.commit")}: ${value(info.commit)}`,
    `${t("version.buildTime")}: ${value(info.build_time)}`,
    `${t("version.startedAt")}: ${value(info.started_at)}`,
    `${t("version.goVersion")}: ${value(info.go_version)}`,
    ...(info.dirty ? [t("version.dirty")] : []),
  ].join("\n");

  return (
    <div
      className="border-t border-slate-200 px-4 py-2 text-xs text-slate-400"
      title={tooltip}
      data-testid="build-info"
    >
      <span className="font-medium text-slate-500">{value(info.version)}</span>
      {info.commit !== "unknown" ? <span className="ml-2">{shortCommit(info.commit)}</span> : null}
      {info.dirty ? <span className="ml-1 text-amber-600">*</span> : null}
    </div>
  );
}