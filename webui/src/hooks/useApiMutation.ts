import { useMutation, useQueryClient, type UseMutationOptions } from "@tanstack/react-query";

import { useToast } from "@/components/ui/Toast";
import type { MessageKey } from "@/i18n/en";
import { useI18n } from "@/i18n";

/** useApiMutation 统一处理写操作后的缓存失效与错误提示。 */
export function useApiMutation<TData, TVars>(
  keys: unknown[][],
  fn: (vars: TVars) => Promise<TData>,
  options?: UseMutationOptions<TData, Error, TVars> & { successKey?: MessageKey },
) {
  const client = useQueryClient();
  const toast = useToast();
  const { t } = useI18n();
  return useMutation<TData, Error, TVars>({
    ...options,
    mutationFn: fn,
    onSuccess: (data, vars, res, ctx) => {
      keys.forEach((key) => client.invalidateQueries({ queryKey: key }));
      if (options?.successKey) toast(t(options.successKey), "success");
      return options?.onSuccess?.(data, vars, res, ctx);
    },
    onError: (err, vars, res, ctx) => {
      toast(err.message);
      return options?.onError?.(err, vars, res, ctx);
    },
  });
}
