import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";

type Toast = { id: number; message: string; kind: "error" | "success" };

const ToastContext = createContext<(message: string, kind?: Toast["kind"]) => void>(() => {});

export function useToast() {
  return useContext(ToastContext);
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);

  const push = useCallback((message: string, kind: Toast["kind"] = "error") => {
    const id = Date.now() + Math.random();
    setItems((prev) => [...prev, { id, message, kind }]);
    setTimeout(() => setItems((prev) => prev.filter((t) => t.id !== id)), 4000);
  }, []);

  const value = useMemo(() => push, [push]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div className="pointer-events-none fixed bottom-4 right-4 z-50 flex flex-col gap-2">
        {items.map((t) => (
          <div
            key={t.id}
            className={
              "pointer-events-auto rounded-lg px-4 py-2 text-sm text-white shadow-lg " +
              (t.kind === "error" ? "bg-red-600" : "bg-emerald-600")
            }
          >
            {t.message}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
