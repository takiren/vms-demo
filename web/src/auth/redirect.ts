// ログイン後に戻る先（?redirect=）を検証する。オープンリダイレクトを防ぐため、
// 同じオリジンの相対パス（"/" 1 つで始まるもの）だけを通し、それ以外は "/" にする。
export const DEFAULT_REDIRECT = "/";
const MAX_LENGTH = 2048;
const PROBE_ORIGIN = "https://redirect.invalid";

function hasControlOrBackslash(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    // ブラウザは URL 中のタブや改行を取り除き、"\" を "/" として扱う（"/\evil.example" など）
    if (c <= 0x1f || c === 0x7f || c === 0x5c) return true;
  }
  return false;
}

export function sanitizeRedirect(raw: unknown): string {
  if (typeof raw !== "string" || raw.length === 0 || raw.length > MAX_LENGTH) {
    return DEFAULT_REDIRECT;
  }
  // "//evil.example" はプロトコル相対 URL。"https://..." や "javascript:" は "/" で始まらない
  if (!raw.startsWith("/") || raw.startsWith("//")) return DEFAULT_REDIRECT;
  if (hasControlOrBackslash(raw)) return DEFAULT_REDIRECT;

  let url: URL;
  try {
    url = new URL(raw, PROBE_ORIGIN);
  } catch {
    return DEFAULT_REDIRECT;
  }
  if (url.origin !== PROBE_ORIGIN) return DEFAULT_REDIRECT;

  // "/%2F%2Fevil.example" のように、デコードするとプロトコル相対になるものも拒否する
  let decoded: string;
  try {
    decoded = decodeURIComponent(url.pathname);
  } catch {
    return DEFAULT_REDIRECT;
  }
  if (decoded.startsWith("//") || hasControlOrBackslash(decoded)) {
    return DEFAULT_REDIRECT;
  }
  // ログイン画面自体には戻さない（ループ防止）
  if (url.pathname === "/login") return DEFAULT_REDIRECT;

  return url.pathname + url.search + url.hash;
}
