# CLAUDE.md — 프런트엔드 (`web/`)

루트 `../CLAUDE.md`의 백엔드·빌드 설명을 보완하는, 프런트엔드 전용 안내서. 주석과
UI 문자열은 대부분 중국어이므로 주변 코드의 관례를 그대로 따를 것.

스택: Next.js 16 (App Router) · React 19 (React Compiler 켜짐) · TypeScript ·
Tailwind v4 · shadcn/radix · Zustand · Biome.

## 명령어 (`web/`에서 실행)

- `npm run dev` — 개발 서버(`:5173` 아님, 기본 Next 포트). 보통은 루트 `./dev.sh`로
  백엔드와 함께 띄운다. `/api/*`는 `next.config.mjs`의 rewrite로 Go 백엔드(`http://localhost:8787`, `AUTOPENTEST_API`로 덮어쓰기)에 프록시된다.
- `npm run build:static` — 정적 내보내기(`NEXT_EXPORT=1`, `output: "export"`). 산출물
  `web/out`을 `server/webui/dist`에 복사해 Go 바이너리에 임베드한다(루트 문서 참고).
- `npm run lint` / `npm run check` — Biome 린트 / 린트+포맷 검사. `npm run check:fix`로
  자동 수정, `npm run format`로 포맷만. **ESLint/Prettier 아님 — Biome 하나로 통일.**
- JS 테스트는 `node:test` 기반의 `*.test.mjs` 몇 개뿐(`src/lib/activity-merge.test.mjs`,
  `chat-mentions.test.mjs`). `node --test src/lib/*.test.mjs`로 실행. 통합 테스트 러너는 없다.
- husky pre-commit 훅이 staged JS/TS에 `biome check --write`를 돌린다.

## 빌드 모드 세 가지 (`next.config.mjs`)

코드는 반드시 이 세 모드에서 모두 동작해야 한다:
1. **dev** — `/api` rewrite + HMR. SSE만은 이 rewrite를 쓰면 안 된다(아래 참고).
2. **static export** (`NEXT_EXPORT=1`) — Node 런타임 없음, 라우트마다 `<route>/index.html`,
   `trailingSlash: true`, 이미지 최적화 off. 프로덕션/Docker 기본.
3. **mock** (`NEXT_PUBLIC_MOCK=1`) — Vercel 데모용. 백엔드 없이 전체가 mock으로 동작.

## 핵심 규약

**API 클라이언트는 `src/lib/api.ts` 하나로 집중.** 컴포넌트에서 `fetch`를 직접 쓰지 말고
`api.*` 메서드를 호출한다. 내부 `http<T>()`가 토큰 주입, `/api` 접두사, 401 처리
(토큰 제거 → `/login` 리다이렉트), 에러 메시지 추출을 담당한다. 반환 타입은
`src/lib/types.ts`의 도메인 타입이며, 백엔드 핸들러의 JSON 모양과 일치시킨다
(백엔드가 다르게 직렬화하는 일부 필드 — 예: unix int인 `created_at` — 는 그대로 통과시키고
호출 지점에서 포맷).

**Mock은 `http()` 진입점에서 분기.** `MOCK`이 참이면 모든 호출이 `src/lib/mock/handler.ts`로
간다. 새 `api.*` 엔드포인트를 추가하면 mock 데모가 깨지지 않도록 mock 핸들러에도
대응을 넣어야 한다.

**SSE(실시간 활동 스트림)는 `fetch`가 아니라 `EventSource`로, `api.sseUrl(path)`로 URL을 만든다.**
- 프로덕션: 백엔드가 UI와 SSE를 같은 포트에서 서빙하므로 **동일 출처(상대 경로)** 가 기본 —
  리버스 프록시(443만 개방) 배포가 이걸로 가능해진다.
- 개발: Next `/api` rewrite는 스트림을 버퍼링해 이벤트가 안 오므로, dev에서만 `:8787`로 직결.
- 토큰은 쿠키가 아니라 `?token=` 쿼리로 붙인다(교차 출처 SSE는 쿠키 불가).
- `NEXT_PUBLIC_SSE_BASE`로 덮어쓸 수 있다(동일 출처 강제는 `""`).

**인증.** 토큰은 `localStorage`와 쿠키(`artex_token`) 양쪽에 저장. 쿠키는 `src/proxy.ts`
(Next middleware)가 서버 측 라우트 가드로 읽어 미로그인 시 `/login`, 로그인 상태로 `/login`·
`/setup` 접근 시 `/function/tasks`로 보낸다. `localStorage` 쪽은 `http()`의 Bearer 헤더용.

**경로/구조.** import 별칭 `@/*` → `src/*`. App Router 라우트 그룹:
`(auth)` 로그인·초기 설정, `(main)` 인증 뒤 앱 전체(`dashboard`, `chat`, `function/*`
= tasks/assets/findings/traffic/…, `system/*`, `_components/sidebar`), `(external)` 공개 페이지.
전역 클라이언트 상태는 `src/stores/`(Zustand, 현재 preferences). 공용 로직은 `src/lib/`,
훅은 `src/hooks/`.

**Biome 규칙 중 주의할 것들** (`biome.json`):
- import 자동 정렬이 켜져 있다(react → next → 패키지 → 별칭 → 상대경로, 그룹 사이 빈 줄).
- `noFloatingPromises`/`noMisusedPromises` error — Promise는 반드시 await/void 처리.
- `useSortedClasses` — Tailwind 클래스 자동 정렬.
- `useFilenamingConvention` error — 파일명 규칙(kebab-case) 강제.
- `noImportCycles` error — 순환 import 금지.
- `src/components/ui`, `src/components/calendar`는 린트/포맷 제외(shadcn 생성 코드).
