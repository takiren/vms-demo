import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, vi } from "vitest";
import { setUnauthorizedHandler } from "../api/client";

// jsdom は scrollTo を実装していない（TanStack Router のスクロール復元が呼ぶ）
window.scrollTo = () => {};

afterEach(() => {
  cleanup();
  setUnauthorizedHandler(undefined);
  vi.unstubAllGlobals();
});
