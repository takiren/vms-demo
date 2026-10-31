# web

Vite と React によるフロントエンドです。カメラの多分割ライブ表示、録画再生、カメラ・ユーザー管理などの画面を提供します。

- Vite、React、TypeScript（strict）
- ルーティング: TanStack Router（コードで定義。`src/router.tsx`）
- データ取得: TanStack Query
- スタイル: Tailwind CSS v4（`@tailwindcss/vite`）
- API の型: openapi-typescript で `../api/openapi.yaml` から `src/api/schema.gen.ts` を生成し、openapi-fetch で呼ぶ
- テスト: Vitest、Testing Library、jsdom

## 必要なもの

- Node.js（`.node-version`）
- pnpm（`package.json` の `packageManager`。`corepack enable` で揃う）

## コマンド

```sh
pnpm install          # 依存関係（pnpm-lock.yaml のとおり）
pnpm dev              # 開発サーバー（:5173）。ブラウザでは https://vms.localhost/ を開く
pnpm generate         # API の型を作り直す（api/openapi.yaml を変えたら必ず実行してコミット）
pnpm lint             # ESLint と Prettier の確認（直すときは pnpm format）
pnpm typecheck        # tsc --noEmit
pnpm test             # Vitest（pnpm test:watch、カバレッジは pnpm coverage）
pnpm build            # 型検査と本番用のビルド（dist/）
```

CI（`.github/workflows/web.yml`）は、install（`--frozen-lockfile`）、生成物の差分、lint、typecheck、テスト（カバレッジ）、build を確認します。

## 開発サーバー

ブラウザは Caddy（`docker compose up`、[deploy/README.md](../deploy/README.md)）経由で `https://vms.localhost/` を開きます。Caddy が `/api` を API（`:8080`）に、それ以外を vite dev（`host.docker.internal:5173`）に振り分けるので、同一オリジンになります。

- セッション Cookie は `__Host-` で `Secure` なので、HTTPS（Caddy）を通さないとログインできません。`http://localhost:5173/` を直接開いても API には届きません
- Caddy のコンテナから届くよう、全インターフェース（`0.0.0.0:5173`）で待ち受けます。Vite は知らない `Host` ヘッダーを拒否するので、`server.allowedHosts` に `VMS_HOST` だけを足しています
- HMR の WebSocket も Caddy の TLS を通ります（`wss://<VMS_HOST>:<CADDY_HTTPS_PORT>/`）
- `VMS_HOST`（既定 `vms.localhost`）と `CADDY_HTTPS_PORT`（既定 443）は、リポジトリ直下の `.env` から読みます。読むのはこの 2 つだけで、`.env` の値がクライアントのコードに入ることはありません

## 構成

| パス                             | 内容                                                                                    |
| -------------------------------- | --------------------------------------------------------------------------------------- |
| `src/api/schema.gen.ts`          | openapi-typescript の生成物。手で編集しない                                             |
| `src/api/client.ts`              | 型付きの API クライアント（`api.GET("/auth/me")` など）。401 でセッション切れを通知する |
| `src/api/errors.ts`              | `ApiError`、`unwrap`、`Retry-After` の解釈、エラーの文言                                |
| `src/auth/`                      | ログイン中のユーザー（`/auth/me` のクエリ）、ログイン後の戻り先の検証                   |
| `src/router.tsx`                 | ルートの定義と認証ガード                                                                |
| `src/app.tsx`                    | QueryClient と Router の組み立て                                                        |
| `src/components/`、`src/routes/` | 画面                                                                                    |

## 認証

- `/auth/me` が 401 なら、ログイン画面（`/login?redirect=<元のパス>`）に移ります
- `redirect` は `sanitizeRedirect` で検証します。同じオリジンの相対パス（`/` 1 つで始まり、`//` や `\`、制御文字を含まないもの）だけを通し、それ以外はトップに戻します（オープンリダイレクトの防止）
- ほかの API が 401 を返したら（セッション切れ）、キャッシュを捨ててログイン画面に移ります
- セッション Cookie は HttpOnly なので JS からは触りません。fetch は `credentials: "same-origin"` です。CSRF は API が `Origin` ヘッダーで確認します
- パスワードやトークンを `localStorage` などに保存しません。パスワードは React の state や TanStack Query のキャッシュにも入れず、送信時にフォームから読みます
- ロールによる表示の切り替え（管理メニュー、管理画面）は見た目だけです。認可は API が操作ごとに行います

## ルーティングをコードで定義する理由

ファイルベースのルーティングは Vite プラグインが `routeTree.gen.ts` を生成し、その生成物もコミットと差分の確認が要ります。画面の数が少ないので、`createRoute` と `getParentRoute` でコードに書いても型（パス、search、context）は同じように付き、テストでもそのまま組み立てられます。
