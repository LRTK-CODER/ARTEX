// Mock 스위치. 빌드 때 넣는 공개 변수다(NEXT_PUBLIC_ 접두사가 있어야 브라우저에서 읽힌다).
// Vercel에서 NEXT_PUBLIC_MOCK=1로 두면 사이트 전체가 백엔드 없이 mock으로 돈다.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
