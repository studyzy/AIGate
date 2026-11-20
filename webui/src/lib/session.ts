// token 只放 sessionStorage：关标签页即失效，也不给 CSRF 留口子。
const TOKEN_KEY = "aigate.ui.token";

export function getToken(): string | null {
  return sessionStorage.getItem(TOKEN_KEY);
}

export function setToken(token: string) {
  sessionStorage.setItem(TOKEN_KEY, token);
}

export function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY);
}
