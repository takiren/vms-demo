/// <reference types="vitest/config" />
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, loadEnv } from "vite";

// 開発時は Caddy（Docker）が https://<VMS_HOST>/ を host.docker.internal:5173 に転送する（deploy/Caddyfile）。
// ホスト名と HTTPS のポートは、リポジトリ直下の .env（compose と同じもの）から読む。
// 読むのはこの 2 つだけ。envDir は変えないので、直下の .env がクライアントのコードに入ることはない。
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, "..", ["VMS_HOST", "CADDY_HTTPS_PORT"]);
  const host = env.VMS_HOST || "vms.localhost";
  const httpsPort = Number(env.CADDY_HTTPS_PORT || "443");

  return {
    plugins: [react(), tailwindcss()],
    server: {
      // Caddy のコンテナから host.docker.internal で届くよう、全インターフェースで待ち受ける
      host: true,
      port: 5173,
      strictPort: true,
      // Vite は知らない Host ヘッダーを拒否する（DNS rebinding 対策）。Caddy 経由のホスト名だけ足す
      allowedHosts: [host],
      // HMR の WebSocket も Caddy の TLS を通す（ブラウザから見たポートは Caddy の HTTPS ポート）
      hmr: {
        protocol: "wss",
        host,
        clientPort: httpsPort,
      },
    },
    preview: {
      port: 4173,
      strictPort: true,
      allowedHosts: [host],
    },
    build: {
      sourcemap: true,
    },
    test: {
      environment: "jsdom",
      setupFiles: ["./src/test/setup.ts"],
      restoreMocks: true,
      coverage: {
        provider: "v8",
        include: ["src/**/*.{ts,tsx}"],
        exclude: [
          "src/api/schema.gen.ts",
          "src/main.tsx",
          "src/test/**",
          "src/**/*.test.{ts,tsx}",
        ],
        reporter: ["text", "text-summary"],
        thresholds: {
          lines: 60,
          functions: 60,
          branches: 60,
          statements: 60,
        },
      },
    },
  };
});
