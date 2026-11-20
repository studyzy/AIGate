import logoMark from "@/assets/logo-mark.svg";

/** Brand 是侧栏与登录页共用的标识：品牌图形 + 应用名。 */
export function Brand({ name, size = "md" }: { name: string; size?: "md" | "lg" }) {
  const box = size === "lg" ? "h-11 w-11" : "h-7 w-7";
  const text = size === "lg" ? "text-lg" : "text-sm";
  return (
    <span className="flex items-center gap-2">
      <img src={logoMark} alt="" aria-hidden="true" className={box} />
      <span className={"font-semibold tracking-tight text-slate-900 " + text}>{name}</span>
    </span>
  );
}
