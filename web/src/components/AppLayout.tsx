import { useQueryClient } from "@tanstack/react-query";
import {
  Link,
  Outlet,
  useRouteContext,
  useRouter,
  type LinkProps,
} from "@tanstack/react-router";
import { useRef, useState, type ReactNode } from "react";
import type { User } from "../api/client";
import { errorMessage } from "../api/errors";
import { logout } from "../auth/session";

const linkClass = "rounded px-3 py-2 text-sm hover:bg-slate-700";
const activeClass = "bg-slate-700 font-semibold";

function NavLink({
  to,
  children,
}: {
  to: LinkProps["to"];
  children: ReactNode;
}) {
  return (
    <Link
      to={to}
      className={linkClass}
      activeProps={{ className: activeClass }}
    >
      {children}
    </Link>
  );
}

// <details> のメニュー。項目を選んだら閉じる
function Menu({ label, children }: { label: ReactNode; children: ReactNode }) {
  const ref = useRef<HTMLDetailsElement>(null);
  return (
    <details ref={ref} className="relative">
      <summary className={`${linkClass} cursor-pointer list-none`}>
        {label}
      </summary>
      <div
        className="absolute right-0 z-10 mt-1 flex min-w-40 flex-col rounded border border-slate-200 bg-white py-1 text-slate-900 shadow"
        onClick={() => ref.current?.removeAttribute("open")}
      >
        {children}
      </div>
    </details>
  );
}

const menuItemClass = "px-4 py-2 text-left text-sm hover:bg-slate-100";

function UserMenu({ me }: { me: User }) {
  const router = useRouter();
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();

  async function onLogout() {
    setError(undefined);
    try {
      await logout();
    } catch (err) {
      setError(errorMessage(err));
      return;
    }
    // 前のユーザーのデータを残さない
    queryClient.clear();
    await router.navigate({ to: "/login", replace: true });
  }

  return (
    <div className="flex items-center gap-2">
      {error && (
        <span role="alert" className="text-sm text-red-300">
          ログアウトできませんでした: {error}
        </span>
      )}
      <Menu
        label={
          <>
            {me.username}
            <span className="ml-1 text-xs text-slate-300">
              （{me.role === "admin" ? "管理者" : "閲覧者"}）
            </span>
          </>
        }
      >
        <Link to="/account/password" className={menuItemClass}>
          パスワードの変更
        </Link>
        <button
          type="button"
          className={menuItemClass}
          onClick={() => void onLogout()}
        >
          ログアウト
        </button>
      </Menu>
    </div>
  );
}

export function AppLayout() {
  const { me } = useRouteContext({ from: "/authed" });
  // ロールでの出し分けは表示だけ。API が操作ごとにロールを確認する
  const isAdmin = me.role === "admin";

  return (
    <div className="min-h-screen">
      <header className="flex flex-wrap items-center justify-between gap-2 bg-slate-800 px-4 py-2 text-white">
        <div className="flex flex-wrap items-center gap-1">
          <Link to="/live" className="mr-4 font-bold">
            VMS
          </Link>
          <nav
            aria-label="メイン"
            className="flex flex-wrap items-center gap-1"
          >
            <NavLink to="/live">多分割ライブ</NavLink>
            <NavLink to="/cameras">カメラ</NavLink>
            <NavLink to="/recordings">録画</NavLink>
            {isAdmin && (
              <Menu label="管理">
                <Link to="/admin/cameras" className={menuItemClass}>
                  カメラ管理
                </Link>
                <Link to="/admin/users" className={menuItemClass}>
                  ユーザー管理
                </Link>
              </Menu>
            )}
          </nav>
        </div>
        <UserMenu me={me} />
      </header>
      <main className="mx-auto max-w-7xl p-4">
        <Outlet />
      </main>
    </div>
  );
}
