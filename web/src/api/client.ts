import createClient, { type Middleware } from "openapi-fetch";
import type { components, paths } from "./schema.gen";

export type User = components["schemas"]["User"];
export type Role = components["schemas"]["Role"];
export type ErrorCode = components["schemas"]["ErrorCode"];

// セッションが切れた（API が 401 を返した）ときに呼ぶ。createApp が登録する。
let unauthorizedHandler: (() => void) | undefined;

export function setUnauthorizedHandler(
  handler: (() => void) | undefined,
): void {
  unauthorizedHandler = handler;
}

// ログインの 401 は「資格情報が違う」、/auth/me の 401 は認証ガードが扱うので除く
const sessionExpiryMiddleware: Middleware = {
  onResponse({ response, schemaPath }) {
    if (
      response.status === 401 &&
      schemaPath !== "/auth/login" &&
      schemaPath !== "/auth/me"
    ) {
      unauthorizedHandler?.();
    }
    return undefined;
  },
};

// API は同一オリジンの /api。セッション Cookie は HttpOnly なので JS からは触らない。
// 状態を変えるリクエストは、ブラウザが付ける Origin で CSRF を確認される（API 側）。
export const api = createClient<paths>({
  baseUrl: `${window.location.origin}/api`,
  credentials: "same-origin",
  // 呼ぶたびに globalThis.fetch を引く（テストで差し替えられるように）
  fetch: (request) => globalThis.fetch(request),
});
api.use(sessionExpiryMiddleware);
