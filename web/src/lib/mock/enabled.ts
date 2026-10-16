// Mock 스위치. 빌드 시점에 주입되는 공개 변수(NEXT_PUBLIC_ 접두사가 붙어야 브라우저에서 읽을 수 있음).
// Vercel에서 NEXT_PUBLIC_MOCK=1로 설정하면 전체 사이트가 mock으로 동작, 백엔드 불필요.
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
