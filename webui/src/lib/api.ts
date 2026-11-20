import { ApiError, UnauthorizedError } from "./errors";
import { clearToken, getToken } from "./session";

export const UNAUTHORIZED_EVENT = "aigate:unauthorized";

type ErrorBody = { error?: { message?: string } };

/** api 统一注入鉴权头、解析错误体；401 清 token 并广播，由路由层跳登录页。 */
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const token = getToken();
  let res: Response;
  try {
    res = await fetch(path, {
      ...init,
      headers: {
        "Content-Type": "application/json",
        ...(token ? { Authorization: "Bearer " + token } : {}),
        ...init.headers,
      },
    });
  } catch (err) {
    throw new ApiError(err instanceof Error ? err.message : "network error", 0);
  }

  if (res.status === 401) {
    clearToken();
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    throw new UnauthorizedError();
  }

  const body = (await res.json().catch(() => null)) as ErrorBody | null;
  if (!res.ok) {
    throw new ApiError(body?.error?.message ?? res.statusText, res.status);
  }
  return body as T;
}

/** downloadFile 带鉴权拉流并触发浏览器下载（导出接口不能走裸 <a href>，需要 Authorization 头）。 */
export async function downloadFile(path: string, filename: string) {
  const token = getToken();
  const res = await fetch(path, {
    headers: token ? { Authorization: "Bearer " + token } : {},
  });
  if (res.status === 401) {
    clearToken();
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    throw new UnauthorizedError();
  }
  if (!res.ok) throw new ApiError(res.statusText || "download failed", res.status);
  const url = URL.createObjectURL(await res.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  link.click();
  URL.revokeObjectURL(url);
}
