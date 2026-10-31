import { describe, expect, it } from "vitest";
import { DEFAULT_REDIRECT, sanitizeRedirect } from "./redirect";

describe("sanitizeRedirect", () => {
  it.each([
    ["/live", "/live"],
    ["/cameras/abc?tab=rec#t=10", "/cameras/abc?tab=rec#t=10"],
    ["/admin/users", "/admin/users"],
    ["/a/../recordings", "/recordings"],
  ])("keeps same-origin path %s", (input, want) => {
    expect(sanitizeRedirect(input)).toBe(want);
  });

  it.each([
    [undefined],
    [null],
    [42],
    [["/live"]],
    [""],
    ["live"],
    ["//evil.example"],
    ["//evil.example/path"],
    ["/\\evil.example"],
    ["\\\\evil.example"],
    ["/\tevil"],
    ["/\n/evil.example"],
    ["https://evil.example/"],
    ["http:/evil.example"],
    ["javascript:alert(1)"],
    ["data:text/html,hi"],
    [" /live"],
    ["/%2F%2Fevil.example"],
    ["/%2f/evil.example"],
    ["/%5Cevil.example"],
    ["/%E0%A4%A"],
    ["/login"],
    ["/login?redirect=/live"],
    ["/" + "a".repeat(3000)],
  ])("rejects %j", (input) => {
    expect(sanitizeRedirect(input)).toBe(DEFAULT_REDIRECT);
  });
});
