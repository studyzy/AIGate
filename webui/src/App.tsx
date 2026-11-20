import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect } from "react";
import { RouterProvider } from "react-router-dom";

import { ToastProvider } from "./components/ui/Toast";
import { useI18n } from "./i18n";
import { setLocale } from "./lib/format";
import { router } from "./router";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
});

// LocaleSync 让 Intl 数字/日期格式化跟随界面语言。
function LocaleSync() {
  const { lang } = useI18n();
  useEffect(() => setLocale(lang), [lang]);
  return null;
}

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <LocaleSync />
        <RouterProvider router={router} />
      </ToastProvider>
    </QueryClientProvider>
  );
}
