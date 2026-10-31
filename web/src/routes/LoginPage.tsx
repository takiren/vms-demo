import { useQueryClient } from "@tanstack/react-query";
import { getRouteApi, useRouter } from "@tanstack/react-router";
import { useState, type FormEvent } from "react";
import { ApiError, formatWait } from "../api/errors";
import { sanitizeRedirect } from "../auth/redirect";
import { login, meQueryKey } from "../auth/session";
import { buttonClass, field, inputClass } from "../components/form";

const route = getRouteApi("/login");

export function loginErrorMessage(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return "サーバーに接続できませんでした。ネットワークを確認してください。";
  }
  switch (err.status) {
    // 400（形式が違う）も 401 と同じ文言にする。どちらが違うかは伝えない
    case 400:
    case 401:
      return "ユーザー名またはパスワードが違います。";
    case 429:
      return err.retryAfterSeconds
        ? `ログインの失敗が続いたため、受け付けを止めています。${formatWait(err.retryAfterSeconds)}後にもう一度お試しください。`
        : "ログインの失敗が続いたため、受け付けを止めています。しばらくしてからもう一度お試しください。";
    case 503:
      return "サーバーが混み合っています。しばらくしてからもう一度お試しください。";
    default:
      return `ログインできませんでした（${err.status}）。`;
  }
}

export function LoginPage() {
  const search = route.useSearch();
  const router = useRouter();
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const [pending, setPending] = useState(false);

  // パスワードは state にもキャッシュ（useMutation の variables）にも残さない。フォームから直接読む
  async function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget;
    const data = new FormData(form);
    setError(undefined);
    setPending(true);
    try {
      const user = await login({
        username: field(data, "username"),
        password: field(data, "password"),
      });
      queryClient.setQueryData(meQueryKey, user);
      await router.navigate({
        href: sanitizeRedirect(search.redirect),
        replace: true,
      });
    } catch (err) {
      setError(loginErrorMessage(err));
      const password = form.elements.namedItem("password");
      if (password instanceof HTMLInputElement) {
        password.value = "";
        password.focus();
      }
    } finally {
      setPending(false);
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center p-4">
      <form
        onSubmit={(e) => void onSubmit(e)}
        className="w-full max-w-sm rounded-lg border border-slate-200 bg-white p-6 shadow-sm"
        aria-labelledby="login-title"
      >
        <h1 id="login-title" className="text-xl font-semibold">
          VMS にログイン
        </h1>
        {error && (
          <p
            role="alert"
            className="mt-4 rounded bg-red-50 px-3 py-2 text-sm text-red-800"
          >
            {error}
          </p>
        )}
        <label className="mt-4 block text-sm font-medium">
          ユーザー名
          <input
            name="username"
            autoComplete="username"
            required
            maxLength={64}
            autoCapitalize="none"
            spellCheck={false}
            className={inputClass}
          />
        </label>
        <label className="mt-4 block text-sm font-medium">
          パスワード
          <input
            name="password"
            type="password"
            autoComplete="current-password"
            required
            maxLength={128}
            className={inputClass}
          />
        </label>
        <button
          type="submit"
          disabled={pending}
          className={`${buttonClass} mt-6 w-full`}
        >
          {pending ? "ログイン中…" : "ログイン"}
        </button>
      </form>
    </main>
  );
}
