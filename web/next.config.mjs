import { fileURLToPath } from "node:url";

// 정적 export: `NEXT_EXPORT=1 next build`가 순수 정적 디렉터리를 web/out에 만든다. nginx 웹 루트에
// 그대로 넣어 돌릴 수 있다. 개발(next dev)은 이 변수를 두지 않아 /api 리버스 프록시와 핫 리로드를 유지한다.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel 데모: 사이트 전체가 mock으로 돌아 백엔드가 없고 /api 리버스 프록시도 필요 없다.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 상위 디렉터리의 lockfile이 루트 디렉터리 추론과 리소스 경로 생성에 영향을 주지 않게 한다.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // 로컬 네트워크(LAN) IP에서 dev 리소스(HMR)에 접근하게 한다. 필요에 따라 더하거나 뺀다.
  // dev 단계에서는 모든 IPv4 출처가 /_next/*와 HMR에 접근할 수 있다(LAN IP가 바뀌어도 영향 없음).
  // 주의: Next는 보안상 맨 "*"를 막으므로 구간별 와일드카드를 쓴다. "*.*.*.*"는 모든 IPv4와 일치한다.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 순수 정적 export: Node 런타임이 없고, 이미지는 최적화하지 않으며, 라우트마다 <route>/index.html을 만든다.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock 데모: 백엔드가 없어 /api 리버스 프록시가 필요 없다.
          images: { unoptimized: true },
        }
      : {
          // 개발: /api/*를 Go 백엔드로 리버스 프록시한다(기본 :8787, AUTOPENTEST_API로 덮어쓸 수 있다).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
