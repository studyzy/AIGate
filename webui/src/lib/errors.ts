// 错误归一：调用方只需区分「未登录」与「其余业务错误」。
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export class UnauthorizedError extends ApiError {
  constructor() {
    super("unauthorized", 401);
    this.name = "UnauthorizedError";
  }
}
