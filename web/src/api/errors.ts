import type { ErrorCode } from "./client";

// API のエラー応答（と、プロキシなどが返した JSON でないエラー）
export class ApiError extends Error {
  readonly status: number;
  readonly code: ErrorCode | undefined;
  readonly retryAfterSeconds: number | undefined;

  constructor(
    status: number,
    code: ErrorCode | undefined,
    message: string,
    retryAfterSeconds?: number,
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

export function isApiError(err: unknown, status?: number): err is ApiError {
  return (
    err instanceof ApiError && (status === undefined || err.status === status)
  );
}

// Retry-After は秒数（API はこちら）か HTTP-date。読めなければ undefined
export function parseRetryAfter(
  value: string | null,
  now: number = Date.now(),
): number | undefined {
  if (value === null) return undefined;
  const v = value.trim();
  if (/^\d+$/.test(v)) {
    const n = Number(v);
    return n >= 1 ? n : undefined;
  }
  const at = Date.parse(v);
  if (Number.isNaN(at)) return undefined;
  const seconds = Math.ceil((at - now) / 1000);
  return seconds >= 1 ? seconds : undefined;
}

function errorBody(
  body: unknown,
): { code: ErrorCode; message: string } | undefined {
  if (typeof body !== "object" || body === null || !("error" in body)) {
    return undefined;
  }
  const e = body.error;
  if (
    typeof e === "object" &&
    e !== null &&
    "code" in e &&
    "message" in e &&
    typeof e.code === "string" &&
    typeof e.message === "string"
  ) {
    return { code: e.code as ErrorCode, message: e.message };
  }
  return undefined;
}

type FetchResult<T> = { data?: T; error?: unknown; response: Response };

// openapi-fetch の結果を、成功なら data、失敗なら ApiError の throw にする
export async function unwrap<T>(promise: Promise<FetchResult<T>>): Promise<T> {
  const { data, error, response } = await promise;
  if (!response.ok || error !== undefined) {
    const body = errorBody(error);
    throw new ApiError(
      response.status,
      body?.code,
      body?.message ?? `HTTP ${response.status}`,
      parseRetryAfter(response.headers.get("Retry-After")),
    );
  }
  return data as T;
}

// 「N 秒」「N 分」
export function formatWait(seconds: number): string {
  if (seconds < 60) return `${seconds} 秒`;
  return `${Math.ceil(seconds / 60)} 分`;
}

// ログイン以外の画面で使う、一般的なエラーの文言
export function errorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return "サーバーに接続できませんでした。ネットワークを確認してください。";
  }
  switch (err.status) {
    case 401:
      return "ログインが必要です。";
    case 403:
      return "この操作をする権限がありません。";
    case 404:
      return "見つかりませんでした。";
    case 429:
      return err.retryAfterSeconds
        ? `リクエストが多すぎます。${formatWait(err.retryAfterSeconds)}後にもう一度お試しください。`
        : "リクエストが多すぎます。しばらくしてからもう一度お試しください。";
    case 503:
      return "サーバーが混み合っています。しばらくしてからもう一度お試しください。";
    default:
      return err.status >= 500
        ? `サーバーでエラーが発生しました（${err.status}）。`
        : `リクエストが正しくありません（${err.status}）。`;
  }
}
