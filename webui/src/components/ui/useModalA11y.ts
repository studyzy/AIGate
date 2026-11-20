import { useEffect, useRef } from "react";

/**
 * useModalA11y 统一模态层的键盘与焦点行为：Esc 关闭、打开时聚焦面板、
 * 关闭后把焦点还给触发元素、并锁住背景滚动。
 */
export function useModalA11y<T extends HTMLElement>(open: boolean, onClose: () => void) {
  const ref = useRef<T>(null);
  const close = useRef(onClose);
  close.current = onClose;

  useEffect(() => {
    if (!open) return;
    const previous = document.activeElement as HTMLElement | null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    ref.current?.focus();

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close.current();
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      document.body.style.overflow = previousOverflow;
      previous?.focus?.();
    };
  }, [open]);

  return ref;
}
