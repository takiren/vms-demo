import { describe, expect, it } from "vitest";
import {
  ApiError,
  errorMessage,
  formatWait,
  parseRetryAfter,
  unwrap,
} from "./errors";

describe("parseRetryAfter", () => {
  it("reads seconds", () => {
    expect(parseRetryAfter("30")).toBe(30);
    expect(parseRetryAfter(" 5 ")).toBe(5);
  });
  it("reads an HTTP date", () => {
    const now = Date.parse("2026-01-01T00:00:00Z");
    expect(parseRetryAfter("Thu, 01 Jan 2026 00:01:00 GMT", now)).toBe(60);
  });
  it.each([
    [null],
    [""],
    ["0"],
    ["-1"],
    ["soon"],
    ["Thu, 01 Jan 2020 00:00:00 GMT"],
  ])("ignores %j", (v) => {
    expect(parseRetryAfter(v)).toBeUndefined();
  });
});

describe("unwrap", () => {
  it("returns data on success", async () => {
    const response = new Response("{}", { status: 200 });
    await expect(
      unwrap(Promise.resolve({ data: { ok: 1 }, response })),
    ).resolves.toEqual({
      ok: 1,
    });
  });

  it("throws ApiError with code and Retry-After", async () => {
    const response = new Response(null, {
      status: 429,
      headers: { "Retry-After": "12" },
    });
    const error = {
      error: { code: "too_many_requests", message: "slow down" },
    };
    const err = await unwrap(Promise.resolve({ error, response })).catch(
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({
      status: 429,
      code: "too_many_requests",
      message: "slow down",
      retryAfterSeconds: 12,
    });
  });

  it("handles a non-JSON error body", async () => {
    const response = new Response(null, { status: 502 });
    const err = await unwrap(
      Promise.resolve({ error: "Bad Gateway", response }),
    ).catch((e: unknown) => e);
    expect(err).toMatchObject({
      status: 502,
      code: undefined,
      message: "HTTP 502",
    });
  });
});

describe("messages", () => {
  it("formats waits", () => {
    expect(formatWait(59)).toBe("59 秒");
    expect(formatWait(61)).toBe("2 分");
  });
  it.each([
    [new TypeError("fetch failed"), "サーバーに接続できませんでした"],
    [new ApiError(403, "forbidden", ""), "権限がありません"],
    [new ApiError(404, "not_found", ""), "見つかりませんでした"],
    [new ApiError(429, "too_many_requests", "", 90), "2 分後"],
    [new ApiError(429, "too_many_requests", ""), "しばらくしてから"],
    [new ApiError(503, "service_unavailable", ""), "混み合っています"],
    [new ApiError(500, "internal", ""), "（500）"],
    [new ApiError(409, "conflict", ""), "（409）"],
    [new ApiError(401, "unauthorized", ""), "ログインが必要です"],
  ])("errorMessage(%o)", (err, want) => {
    expect(errorMessage(err)).toContain(want);
  });
});
