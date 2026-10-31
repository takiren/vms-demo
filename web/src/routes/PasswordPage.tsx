import { useState, type FormEvent } from "react";
import { ApiError, errorMessage } from "../api/errors";
import {
  changeOwnPassword,
  PASSWORD_MAX,
  PASSWORD_MIN,
  passwordLength,
} from "../auth/session";
import { buttonClass, field, inputClass } from "../components/form";

// 送る前の確認。API も同じ条件で検証する
export function validateNewPassword(
  next: string,
  confirm: string,
): string | undefined {
  const n = passwordLength(next);
  if (n < PASSWORD_MIN || n > PASSWORD_MAX) {
    return `新しいパスワードは ${PASSWORD_MIN} 文字以上 ${PASSWORD_MAX} 文字以下にしてください。`;
  }
  if (next !== confirm) return "確認用のパスワードが一致しません。";
  return undefined;
}

function changeErrorMessage(err: unknown): string {
  if (err instanceof ApiError && err.status === 400) {
    return "現在のパスワードが違うか、新しいパスワードが条件を満たしていません。";
  }
  return errorMessage(err);
}

type Status =
  { kind: "idle" } | { kind: "error"; message: string } | { kind: "done" };

export function PasswordPage() {
  const [status, setStatus] = useState<Status>({ kind: "idle" });
  const [pending, setPending] = useState(false);

  async function onSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const form = e.currentTarget;
    const data = new FormData(form);
    const current = field(data, "current_password");
    const next = field(data, "new_password");
    const invalid = validateNewPassword(next, field(data, "confirm_password"));
    if (invalid) {
      setStatus({ kind: "error", message: invalid });
      return;
    }
    setPending(true);
    try {
      await changeOwnPassword({
        current_password: current,
        new_password: next,
      });
      form.reset();
      setStatus({ kind: "done" });
    } catch (err) {
      setStatus({ kind: "error", message: changeErrorMessage(err) });
    } finally {
      setPending(false);
    }
  }

  return (
    <section className="max-w-md">
      <h1 className="text-xl font-semibold">パスワードの変更</h1>
      <p className="mt-2 text-sm text-slate-600">
        変更すると、ほかの端末のセッションはすべてログアウトされます。
      </p>
      {status.kind === "error" && (
        <p
          role="alert"
          className="mt-4 rounded bg-red-50 px-3 py-2 text-sm text-red-800"
        >
          {status.message}
        </p>
      )}
      {status.kind === "done" && (
        <p
          role="status"
          className="mt-4 rounded bg-green-50 px-3 py-2 text-sm text-green-800"
        >
          パスワードを変更しました。
        </p>
      )}
      <form onSubmit={(e) => void onSubmit(e)} className="mt-4">
        <label className="block text-sm font-medium">
          現在のパスワード
          <input
            name="current_password"
            type="password"
            autoComplete="current-password"
            required
            maxLength={PASSWORD_MAX}
            className={inputClass}
          />
        </label>
        <label className="mt-4 block text-sm font-medium">
          新しいパスワード（{PASSWORD_MIN} 文字以上）
          <input
            name="new_password"
            type="password"
            autoComplete="new-password"
            required
            className={inputClass}
          />
        </label>
        <label className="mt-4 block text-sm font-medium">
          新しいパスワード（確認）
          <input
            name="confirm_password"
            type="password"
            autoComplete="new-password"
            required
            className={inputClass}
          />
        </label>
        <button
          type="submit"
          disabled={pending}
          className={`${buttonClass} mt-6`}
        >
          {pending ? "変更中…" : "変更する"}
        </button>
      </form>
    </section>
  );
}
