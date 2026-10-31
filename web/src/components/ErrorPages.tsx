import { Link, type ErrorComponentProps } from "@tanstack/react-router";
import { errorMessage } from "../api/errors";

// admin 以外が管理画面を開いたとき（表示の制御。API も 403 を返す）
export class ForbiddenError extends Error {
  constructor() {
    super("forbidden");
    this.name = "ForbiddenError";
  }
}

export function NotFoundPage() {
  return (
    <section className="p-6">
      <h1 className="text-xl font-semibold">ページが見つかりません</h1>
      <p className="mt-2">
        <Link to="/" className="text-blue-700 underline">
          トップに戻る
        </Link>
      </p>
    </section>
  );
}

export function ErrorPage({ error, reset }: ErrorComponentProps) {
  if (error instanceof ForbiddenError) {
    return (
      <section role="alert">
        <h1 className="text-xl font-semibold">権限がありません</h1>
        <p className="mt-2 text-sm text-slate-600">
          この画面は管理者だけが使えます。
        </p>
      </section>
    );
  }
  return (
    <section role="alert" className="p-6">
      <h1 className="text-xl font-semibold">エラーが発生しました</h1>
      <p className="mt-2 text-sm text-slate-600">{errorMessage(error)}</p>
      <button
        type="button"
        onClick={reset}
        className="mt-4 rounded border border-slate-300 px-3 py-1 text-sm"
      >
        再読み込み
      </button>
    </section>
  );
}
