import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider, type RouterHistory } from "@tanstack/react-router";
import { setUnauthorizedHandler } from "./api/client";
import { isApiError } from "./api/errors";
import { meQueryKey } from "./auth/session";
import { createAppRouter, type AppRouter } from "./router";

export interface App {
  queryClient: QueryClient;
  router: AppRouter;
}

// 4xx は再試行しても結果が変わらない
function shouldRetry(failureCount: number, err: unknown): boolean {
  if (isApiError(err) && err.status < 500) return false;
  return failureCount < 2;
}

export function createApp(options: { history?: RouterHistory } = {}): App {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: shouldRetry },
      mutations: { retry: false },
    },
  });
  const router = createAppRouter(queryClient, options.history);

  // セッション切れ: 前のユーザーのデータを捨ててログイン画面へ（戻り先を付ける）
  setUnauthorizedHandler(() => {
    const location = router.state.location;
    queryClient.clear();
    queryClient.setQueryData(meQueryKey, null);
    if (location.pathname === "/login") return;
    void router.navigate({
      to: "/login",
      search: { redirect: location.href },
      replace: true,
    });
  });

  return { queryClient, router };
}

export function AppRoot({ app }: { app: App }) {
  return (
    <QueryClientProvider client={app.queryClient}>
      <RouterProvider router={app.router} />
    </QueryClientProvider>
  );
}
