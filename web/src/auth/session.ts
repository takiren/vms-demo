import { queryOptions } from "@tanstack/react-query";
import { api, type User } from "../api/client";
import { isApiError, unwrap } from "../api/errors";
import type { components } from "../api/schema.gen";

export const meQueryKey = ["auth", "me"] as const;

// ログインしていなければ null（401 はエラーにしない）
async function fetchMe(): Promise<User | null> {
  try {
    return await unwrap(api.GET("/auth/me"));
  } catch (err) {
    if (isApiError(err, 401)) return null;
    throw err;
  }
}

// ログイン中のユーザー。ロールは画面の出し分けにだけ使う（認可は API が行う）
export function meQueryOptions() {
  return queryOptions({
    queryKey: meQueryKey,
    queryFn: fetchMe,
    staleTime: Infinity,
  });
}

export type LoginRequest = components["schemas"]["LoginRequest"];
export type ChangePasswordRequest =
  components["schemas"]["ChangePasswordRequest"];

export function login(body: LoginRequest): Promise<User> {
  return unwrap(api.POST("/auth/login", { body }));
}

export async function logout(): Promise<void> {
  await unwrap(api.POST("/auth/logout"));
}

export async function changeOwnPassword(
  body: ChangePasswordRequest,
): Promise<void> {
  await unwrap(api.POST("/auth/password", { body }));
}

// パスワードの長さは Unicode のコードポイントで数える（API と同じ）
export const PASSWORD_MIN = 12;
export const PASSWORD_MAX = 128;

export function passwordLength(s: string): number {
  return [...s].length;
}
