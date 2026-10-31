import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { api } from "./api/client";
import { apiError, json, mockFetch, noContent, user } from "./test/fetchMock";
import { currentLocation, renderApp } from "./test/renderApp";

const unauthorized = () => apiError(401, "unauthorized");

describe("auth guard", () => {
  it("redirects to /login with the original path when not logged in", async () => {
    mockFetch({ "GET /api/auth/me": unauthorized });
    const { app } = await renderApp("/recordings?day=2026-01-01");

    expect(
      await screen.findByRole("heading", { name: "VMS にログイン" }),
    ).toBeInTheDocument();
    expect(currentLocation(app)).toEqual({
      pathname: "/login",
      search: { redirect: "/recordings?day=2026-01-01" },
    });
  });

  it("omits the redirect for the top page", async () => {
    mockFetch({ "GET /api/auth/me": unauthorized });
    const { app } = await renderApp("/");
    await screen.findByRole("heading", { name: "VMS にログイン" });
    expect(currentLocation(app)).toEqual({ pathname: "/login", search: {} });
  });

  it("shows the app when logged in", async () => {
    mockFetch({ "GET /api/auth/me": () => json(200, user("viewer", "bob")) });
    const { app } = await renderApp("/");
    expect(
      await screen.findByRole("heading", { name: "多分割ライブ" }),
    ).toBeInTheDocument();
    expect(screen.getByText("bob")).toBeInTheDocument();
    expect(currentLocation(app).pathname).toBe("/live");
  });

  it("shows an error page when the API is down (does not treat it as logged out)", async () => {
    mockFetch({
      "GET /api/auth/me": () => apiError(503, "service_unavailable"),
    });
    await renderApp("/live");
    expect(
      await screen.findByRole("heading", { name: "エラーが発生しました" }),
    ).toBeInTheDocument();
  });

  it("sends the session cookie same-origin only", async () => {
    const { calls } = mockFetch({
      "GET /api/auth/me": () => json(200, user()),
    });
    await renderApp("/live");
    await screen.findByRole("heading", { name: "多分割ライブ" });
    expect(calls[0]?.credentials).toBe("same-origin");
  });

  it("redirects a logged-in user away from /login", async () => {
    mockFetch({ "GET /api/auth/me": () => json(200, user()) });
    const { app } = await renderApp("/login?redirect=%2Frecordings");
    await screen.findByRole("heading", { name: "録画" });
    expect(currentLocation(app).pathname).toBe("/recordings");
  });

  it("goes to the login page when another API call returns 401", async () => {
    let expired = false;
    mockFetch({
      "GET /api/auth/me": () => (expired ? unauthorized() : json(200, user())),
      "GET /api/users": unauthorized,
    });
    const { app } = await renderApp("/recordings");
    await screen.findByRole("heading", { name: "録画" });

    expired = true;
    await api.GET("/users");
    await screen.findByRole("heading", { name: "VMS にログイン" });
    expect(currentLocation(app)).toEqual({
      pathname: "/login",
      search: { redirect: "/recordings" },
    });
    // 前のユーザーの情報はキャッシュに残さない
    expect(app.queryClient.getQueryData(["auth", "me"])).toBeNull();
  });
});

describe("login form", () => {
  async function submit(username: string, password: string) {
    const u = userEvent.setup();
    await u.type(screen.getByLabelText("ユーザー名"), username);
    await u.type(screen.getByLabelText("パスワード"), password);
    await u.click(screen.getByRole("button", { name: "ログイン" }));
  }

  it("logs in and goes back to the redirect target", async () => {
    let loggedIn = false;
    const { calls } = mockFetch({
      "GET /api/auth/me": () => (loggedIn ? json(200, user()) : unauthorized()),
      "POST /api/auth/login": async (req) => {
        expect(await req.json()).toEqual({
          username: "alice",
          password: "correct horse",
        });
        loggedIn = true;
        return json(200, user());
      },
    });
    const { app } = await renderApp("/login?redirect=%2Fcameras");
    await screen.findByRole("heading", { name: "VMS にログイン" });
    await submit("alice", "correct horse");

    await screen.findByRole("heading", { name: "カメラ" });
    expect(currentLocation(app).pathname).toBe("/cameras");
    const loginCall = calls.find((c) => c.url.endsWith("/api/auth/login"));
    expect(loginCall?.credentials).toBe("same-origin");
  });

  it("ignores an open redirect", async () => {
    mockFetch({
      "GET /api/auth/me": unauthorized,
      "POST /api/auth/login": () => json(200, user()),
    });
    const { app } = await renderApp("/login?redirect=%2F%2Fevil.example%2Fx");
    await screen.findByRole("heading", { name: "VMS にログイン" });
    await submit("alice", "correct horse");

    await screen.findByRole("heading", { name: "多分割ライブ" });
    expect(currentLocation(app).pathname).toBe("/live");
  });

  it("shows a generic message on 401 and clears the password", async () => {
    mockFetch({
      "GET /api/auth/me": unauthorized,
      "POST /api/auth/login": () =>
        apiError(401, "unauthorized", "invalid credentials"),
    });
    await renderApp("/login");
    await screen.findByRole("heading", { name: "VMS にログイン" });
    await submit("alice", "wrong");

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "ユーザー名またはパスワードが違います。",
    );
    expect(screen.getByLabelText("パスワード")).toHaveValue("");
    expect(screen.getByLabelText("ユーザー名")).toHaveValue("alice");
  });

  it("shows the wait time on 429", async () => {
    mockFetch({
      "GET /api/auth/me": unauthorized,
      "POST /api/auth/login": () =>
        apiError(429, "too_many_requests", "too many", { "Retry-After": "30" }),
    });
    await renderApp("/login");
    await screen.findByRole("heading", { name: "VMS にログイン" });
    await submit("alice", "wrong");

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "30 秒後にもう一度お試しください",
    );
  });

  it("shows a connection error when fetch fails", async () => {
    mockFetch({
      "GET /api/auth/me": unauthorized,
      "POST /api/auth/login": () => {
        throw new TypeError("Failed to fetch");
      },
    });
    await renderApp("/login");
    await screen.findByRole("heading", { name: "VMS にログイン" });
    await submit("alice", "pw");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "サーバーに接続できませんでした",
    );
  });
});

describe("layout", () => {
  it("hides the admin menu from viewers", async () => {
    mockFetch({ "GET /api/auth/me": () => json(200, user("viewer")) });
    await renderApp("/live");
    const nav = await screen.findByRole("navigation", { name: "メイン" });
    expect(
      within(nav).getByRole("link", { name: "多分割ライブ" }),
    ).toBeInTheDocument();
    expect(
      within(nav).getByRole("link", { name: "カメラ" }),
    ).toBeInTheDocument();
    expect(within(nav).getByRole("link", { name: "録画" })).toBeInTheDocument();
    expect(within(nav).queryByText("管理")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "ユーザー管理" }),
    ).not.toBeInTheDocument();
  });

  it("shows the admin menu to admins", async () => {
    mockFetch({ "GET /api/auth/me": () => json(200, user("admin")) });
    const { app } = await renderApp("/live");
    const nav = await screen.findByRole("navigation", { name: "メイン" });
    const u = userEvent.setup();
    await u.click(within(nav).getByText("管理"));
    await u.click(within(nav).getByRole("link", { name: "ユーザー管理" }));
    await screen.findByRole("heading", { name: "ユーザー管理" });
    expect(currentLocation(app).pathname).toBe("/admin/users");
    expect(
      within(nav).getByRole("link", { name: "カメラ管理" }),
    ).toBeInTheDocument();
  });

  it("blocks viewers from admin pages", async () => {
    mockFetch({ "GET /api/auth/me": () => json(200, user("viewer")) });
    await renderApp("/admin/users");
    expect(
      await screen.findByRole("heading", { name: "権限がありません" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "ユーザー管理" }),
    ).not.toBeInTheDocument();
  });

  it("shows the camera page placeholder with its id", async () => {
    mockFetch({ "GET /api/auth/me": () => json(200, user()) });
    await renderApp("/cameras/cam-1");
    expect(
      await screen.findByRole("heading", { name: "カメラ単画面" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/cam-1/)).toBeInTheDocument();
  });

  it("shows not found for unknown paths", async () => {
    mockFetch({ "GET /api/auth/me": () => json(200, user()) });
    await renderApp("/nope");
    expect(
      await screen.findByRole("heading", { name: "ページが見つかりません" }),
    ).toBeInTheDocument();
  });

  it("logs out and returns to the login page", async () => {
    let loggedIn = true;
    const { calls } = mockFetch({
      "GET /api/auth/me": () => (loggedIn ? json(200, user()) : unauthorized()),
      "POST /api/auth/logout": () => {
        loggedIn = false;
        return noContent();
      },
    });
    const { app } = await renderApp("/live");
    await screen.findByRole("heading", { name: "多分割ライブ" });
    const u = userEvent.setup();
    await u.click(screen.getByText("alice"));
    await u.click(screen.getByRole("button", { name: "ログアウト" }));

    await screen.findByRole("heading", { name: "VMS にログイン" });
    expect(currentLocation(app)).toEqual({ pathname: "/login", search: {} });
    expect(
      calls.some(
        (c) => c.method === "POST" && c.url.endsWith("/api/auth/logout"),
      ),
    ).toBe(true);
  });

  it("shows an error when logout fails", async () => {
    mockFetch({
      "GET /api/auth/me": () => json(200, user()),
      "POST /api/auth/logout": () => apiError(500, "internal"),
    });
    await renderApp("/live");
    await screen.findByRole("heading", { name: "多分割ライブ" });
    const u = userEvent.setup();
    await u.click(screen.getByText("alice"));
    await u.click(screen.getByRole("button", { name: "ログアウト" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "ログアウトできませんでした",
    );
  });
});

describe("password change", () => {
  async function fill(current: string, next: string, confirm: string) {
    const u = userEvent.setup();
    await u.type(screen.getByLabelText("現在のパスワード"), current);
    await u.type(screen.getByLabelText(/^新しいパスワード（12/), next);
    await u.type(screen.getByLabelText("新しいパスワード（確認）"), confirm);
    await u.click(screen.getByRole("button", { name: "変更する" }));
  }

  it("validates before sending", async () => {
    const { calls } = mockFetch({
      "GET /api/auth/me": () => json(200, user()),
    });
    await renderApp("/account/password");
    await screen.findByRole("heading", { name: "パスワードの変更" });

    await fill("old", "short", "short");
    expect(await screen.findByRole("alert")).toHaveTextContent("12 文字以上");

    await userEvent.clear(screen.getByLabelText(/^新しいパスワード（12/));
    await userEvent.clear(screen.getByLabelText("新しいパスワード（確認）"));
    await userEvent.clear(screen.getByLabelText("現在のパスワード"));
    await fill("old", "long enough pass", "long enough pasz");
    expect(await screen.findByRole("alert")).toHaveTextContent("一致しません");
    expect(calls.filter((c) => c.method === "POST")).toHaveLength(0);
  });

  it("changes the password", async () => {
    mockFetch({
      "GET /api/auth/me": () => json(200, user()),
      "POST /api/auth/password": async (req) => {
        expect(await req.json()).toEqual({
          current_password: "old password",
          new_password: "new password 123",
        });
        return noContent();
      },
    });
    await renderApp("/account/password");
    await screen.findByRole("heading", { name: "パスワードの変更" });
    await fill("old password", "new password 123", "new password 123");
    expect(await screen.findByRole("status")).toHaveTextContent("変更しました");
    expect(screen.getByLabelText("現在のパスワード")).toHaveValue("");
  });

  it("reports a wrong current password", async () => {
    mockFetch({
      "GET /api/auth/me": () => json(200, user()),
      "POST /api/auth/password": () => apiError(400, "bad_request"),
    });
    await renderApp("/account/password");
    await screen.findByRole("heading", { name: "パスワードの変更" });
    await fill("wrong", "new password 123", "new password 123");
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "現在のパスワードが違う",
      ),
    );
  });
});
