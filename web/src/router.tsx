import type { QueryClient } from "@tanstack/react-query";
import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  Outlet,
  redirect,
  type RouterHistory,
} from "@tanstack/react-router";
import type { User } from "./api/client";
import { sanitizeRedirect } from "./auth/redirect";
import { meQueryOptions } from "./auth/session";
import { AppLayout } from "./components/AppLayout";
import {
  ErrorPage,
  ForbiddenError,
  NotFoundPage,
} from "./components/ErrorPages";
import { PlaceholderPage } from "./components/PlaceholderPage";
import { LoginPage } from "./routes/LoginPage";
import { PasswordPage } from "./routes/PasswordPage";

// ルートはコードで定義する（ファイルベースの生成物を持たないため）。
// createRoute の getParentRoute で親子をつなぐと、パスや search、context の型が付く。

export interface RouterContext {
  queryClient: QueryClient;
}

export interface LoginSearch {
  redirect?: string;
}

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: Outlet,
  notFoundComponent: NotFoundPage,
  errorComponent: ErrorPage,
});

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "login",
  validateSearch: (search: Record<string, unknown>): LoginSearch =>
    typeof search.redirect === "string" ? { redirect: search.redirect } : {},
  beforeLoad: async ({ context, search }) => {
    // ログイン済みなら戻り先へ。キャッシュではなく毎回確かめる（ログアウト直後など）
    const me = await context.queryClient
      .fetchQuery({ ...meQueryOptions(), staleTime: 0 })
      .catch(() => null);
    if (me) {
      throw redirect({
        href: sanitizeRedirect(search.redirect),
        replace: true,
      });
    }
  },
  component: LoginPage,
});

// ログインが要る画面の親。/auth/me が 401 ならログイン画面へ
const authedRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "authed",
  beforeLoad: async ({ context, location }): Promise<{ me: User }> => {
    const me = await context.queryClient.ensureQueryData(meQueryOptions());
    if (!me) {
      throw redirect({
        to: "/login",
        search: location.href === "/" ? {} : { redirect: location.href },
        replace: true,
      });
    }
    return { me };
  },
  component: AppLayout,
});

const indexRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/live", replace: true });
  },
});

const liveRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "live",
  component: () => (
    <PlaceholderPage
      title="多分割ライブ"
      note="1 / 4 / 9 / 16 分割のライブ表示（#16）"
    />
  ),
});

const camerasRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "cameras",
  component: () => (
    <PlaceholderPage
      title="カメラ"
      note="カメラを選んで単画面で表示する（#16、#17）"
    />
  ),
});

const cameraRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "cameras/$cameraId",
  component: function CameraPage() {
    const { cameraId } = cameraRoute.useParams();
    return (
      <PlaceholderPage
        title="カメラ単画面"
        note={`ライブと録画再生の切り替え、タイムライン（#16、#17）。カメラ: ${cameraId}`}
      />
    );
  },
});

const recordingsRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "recordings",
  component: () => (
    <PlaceholderPage title="録画" note="録画の検索、再生、切り出し（#17）" />
  ),
});

const passwordRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "account/password",
  component: PasswordPage,
});

// admin だけの画面。表示の制御だけで、本当の認可は API が行う
const adminRoute = createRoute({
  getParentRoute: () => authedRoute,
  path: "admin",
  beforeLoad: ({ context }) => {
    if (context.me.role !== "admin") throw new ForbiddenError();
  },
  errorComponent: ErrorPage,
});

const adminCamerasRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "cameras",
  component: () => (
    <PlaceholderPage
      title="カメラ管理"
      note="カメラの追加、編集、削除（#18）"
    />
  ),
});

const adminUsersRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "users",
  component: () => (
    <PlaceholderPage
      title="ユーザー管理"
      note="ユーザーの追加、ロールとパスワードの変更、削除（#18）"
    />
  ),
});

const routeTree = rootRoute.addChildren([
  loginRoute,
  authedRoute.addChildren([
    indexRoute,
    liveRoute,
    camerasRoute,
    cameraRoute,
    recordingsRoute,
    passwordRoute,
    adminRoute.addChildren([adminCamerasRoute, adminUsersRoute]),
  ]),
]);

export function createAppRouter(
  queryClient: QueryClient,
  history?: RouterHistory,
) {
  return createRouter({
    routeTree,
    context: { queryClient },
    defaultPreload: "intent",
    // ルートのデータは TanStack Query がキャッシュする
    defaultPreloadStaleTime: 0,
    scrollRestoration: true,
    ...(history ? { history } : {}),
  });
}

export type AppRouter = ReturnType<typeof createAppRouter>;

declare module "@tanstack/react-router" {
  interface Register {
    router: AppRouter;
  }
}
