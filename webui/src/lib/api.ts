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
