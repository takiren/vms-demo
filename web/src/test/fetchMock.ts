import { vi } from "vitest";
import type { User } from "../api/client";

type Handler = (request: Request) => Response | Promise<Response>;

// "POST /api/auth/login" のようなキーで、fetch の応答を決める。登録のないものは 599 で失敗させる
export function mockFetch(routes: Record<string, Handler>) {
  const calls: Request[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : new Request(input, init);
    calls.push(request);
    const key = `${request.method} ${new URL(request.url).pathname}`;
    const handler = routes[key];
    if (!handler)
      return json(599, {
        error: { code: "internal", message: `unmocked ${key}` },
      });
    return handler(request);
  });
  vi.stubGlobal("fetch", fn);
  return { fn, calls };
}

export function json(
  status: number,
  body: unknown,
  headers: Record<string, string> = {},
) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

export function noContent() {
  return new Response(null, { status: 204 });
}

export function apiError(
  status: number,
  code: string,
  message = code,
  headers: Record<string, string> = {},
) {
  return json(status, { error: { code, message } }, headers);
}

export function user(role: User["role"] = "viewer", username = "alice"): User {
  return {
    id: "01920000-0000-7000-8000-000000000001",
    username,
    role,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}
