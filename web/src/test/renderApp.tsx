import { createMemoryHistory } from "@tanstack/react-router";
import { render, screen } from "@testing-library/react";
import { AppRoot, createApp } from "../app";

export async function renderApp(path: string) {
  const app = createApp({
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  const result = render(<AppRoot app={app} />);
  // 最初の読み込み（beforeLoad）が終わるまで待つ
  await app.router.load();
  return { app, ...result, screen };
}

export function currentLocation(app: ReturnType<typeof createApp>) {
  const { pathname, search } = app.router.state.location;
  return { pathname, search: search as Record<string, unknown> };
}
