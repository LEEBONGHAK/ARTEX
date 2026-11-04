# 상류 테스트 실패 분석: `TestGraphOverviewExpandsAssociatedCompanyScope`

> 작성일: 2026-11-04
> 분석 브랜치: `i18n/korean` (cb93093) ↔ `main` (b55ceb1) 교차검증
> 분류: **상류(upstream) 사전 존재 결함 — 한국어화와 무관**

## 1. 요약 (결론 먼저)

`agent` 패키지의 `TestGraphOverviewExpandsAssociatedCompanyScope` 테스트는
**번역 작업과 무관하게 상류 `main`에서도 동일하게 실패**한다.

- **근본 원인**: 리팩터 커밋 `06a43f3`(2026-09-24, `refactor(graph_overview): 精简态势概览字段`)가
  프로덕션 코드 `agent/tools.go`의 `graphOverviewData()`에서 `coverage.scope` 필드
  생성 블록을 **의도적으로 삭제**했으나, 그 필드를 단언하는 테스트
  `agent/blackboard_inheritance_test.go`는 함께 수정하지 않았다.
- 그 결과 테스트는 더 이상 생성되지 않는 `coverage["scope"]`를 기대하며
  `blackboard_inheritance_test.go:87`에서 `task scope missing: <nil>`로 실패한다.
- 저장소에 **테스트 CI가 없어서**(`release.yml`은 `v*` 태그 전용 빌드/릴리스만 수행)
  이 stale 테스트가 2026-09-24 이후 지금까지 아무에게도 발견되지 않았다.

→ 한국어화 브랜치가 유발한 회귀가 아니다. 뒤의 §5 교차검증 참조.

## 2. 증상

```
=== RUN   TestGraphOverviewExpandsAssociatedCompanyScope
    blackboard_inheritance_test.go:87: task scope missing: <nil>
--- FAIL: TestGraphOverviewExpandsAssociatedCompanyScope
```

실패 지점 (`agent/blackboard_inheritance_test.go:80-105`):

```go
overview := tools.graphOverviewData()
coverage, ok := overview["coverage"].(map[string]any)   // OK: coverage 블록은 존재
...
scopeRows, ok := coverage["scope"].([]map[string]any)   // ← coverage["scope"] == nil
if !ok || len(scopeRows) != 1 {
    t.Fatalf("task scope missing: %#v", coverage["scope"])   // :87 여기서 실패
}
companyScope, ok := scopeRows[0]["company_scope"].([]map[string]any)   // 도달 못 함
...
keywords, ok := scopeRows[0]["company_keywords"].([]string)           // 도달 못 함
```

`coverage` 맵 자체는 존재하지만 그 안에 `scope` 키가 없어 타입 단언이 실패하고,
`coverage["scope"]`가 `<nil>`로 출력된다.

## 3. 근본 원인

현재 `agent/tools.go`의 `graphOverviewData()`가 만드는 `coverage` 맵(대략 524-549행)이
설정하는 키는 다음뿐이다:

- `denominator`, `tested`, `by_type`, `note`, `pct`/`status`, `host_count`

**`scope` 키는 전혀 설정하지 않는다.** 저장소 전체에서 `company_scope` /
`company_keywords`를 생성하는 프로덕션 코드도 현재 존재하지 않는다
(테스트 파일에만 남아 있음).

### 원인 커밋

```
06a43f3  2026-09-24  refactor(graph_overview): 精简态势概览字段
```

커밋 본문에 명시적으로 기록돼 있다:

> - 删除 coverage.scope 字段（范围根资产 + company 富化）
>   (= coverage.scope 필드 삭제 — 범위 루트 자산 + company 富화)

이 커밋이 `agent/tools.go`에서 제거한 블록(`git show 06a43f3 -- agent/tools.go`):

```go
// scope：当前测试范围的根资产（task_scope 原始行）...
if rows, err := t.as.ListTaskScopeWithSources(t.taskID); err == nil && len(rows) > 0 {
    scope := make([]map[string]any, 0, len(rows))
    ...
        keywords := make([]string, 0)
        ...
            if rule.Kind == "keyword" && rule.Raw != "" {
                keywords = append(keywords, rule.Raw)
            ...
            e["company_scope"] = companyScope
        ...
        if len(keywords) > 0 {
            e["company_keywords"] = keywords
        ...
    scope = append(scope, e)
    m["scope"] = scope
}
```

삭제된 것: `ListTaskScopeWithSources(t.taskID)`로 task_scope 원본 행을 읽어 각 행에
`company_scope`(연관 회사의 범위 규칙)와 `company_keywords`(키워드 규칙)를 富화한 뒤
`coverage["scope"]` 배열로 싣던 로직 전체. **이것이 테스트가 단언하는 바로 그 세 필드다.**

### 왜 테스트가 같이 안 고쳐졌나

같은 커밋 `06a43f3`은 `agent/blackboard_inheritance_test.go`를 건드리지 않았다
(`git show --stat 06a43f3`에 해당 파일 없음). 프로덕션 필드만 지우고 그것을 검증하는
테스트는 남겨, 그 시점부터 테스트가 깨진 채로 방치됐다.

## 4. 타임라인

| 날짜 | 커밋 | 사건 |
|---|---|---|
| 2026-08-21 | `d3183db` | `TestGraphOverviewExpandsAssociatedCompanyScope` 추가 — 당시엔 프로덕션이 `coverage.scope`를 **실제로 생성**했으므로 통과 |
| 2026-09-24 | `06a43f3` | 리팩터로 `coverage.scope` 생성 로직 **삭제**, 테스트는 미수정 → **이 시점부터 테스트 실패 시작** |
| 2026-10-04 | `b55ceb1` | 현재 `main` tip. 여전히 실패 상태 |
| 2026-11-03~04 | `cb93093` | `i18n/korean` tip. `main`을 base로 하며 동일하게 실패 |

`d3183db`는 `06a43f3`의 조상임을 확인
(`git merge-base --is-ancestor d3183db 06a43f3` → 참). 즉 "필드가 있던 시절 추가된
테스트"가 "필드를 지운 리팩터" 이후로 stale이 된 전형적 사례다.

**발각되지 않은 이유**: 이 저장소의 유일한 워크플로 `.github/workflows/release.yml`은
`v*` 태그 push에만 반응해 5개 플랫폼 바이너리와 Docker 이미지를 빌드할 뿐,
`go test`/lint 잡이 없다. 즉 **테스트 CI가 존재하지 않아** 깨진 단위 테스트가
자동으로 드러나지 않았다.

## 5. 교차검증 (한국어화 무관 증명)

동일 환경(WSL kali-linux + Docker `postgres:16-alpine`, port 55432)에서
`main`을 `git worktree`로 체크아웃해 나란히 실행:

```
# main (b55ceb1, 번역 전)
--- FAIL: TestGraphOverviewExpandsAssociatedCompanyScope
    blackboard_inheritance_test.go:87: task scope missing: <nil>

# i18n/korean (cb93093, 번역 후)
--- FAIL: TestGraphOverviewExpandsAssociatedCompanyScope
    blackboard_inheritance_test.go:87: task scope missing: <nil>
```

- `agent` 패키지 전체(`go test ./agent -p 1`)를 양쪽에서 돌려 **실패 목록이 완전히 동일**
  (양쪽 모두 이 한 건만 실패).
- `git diff main i18n/korean -- agent/blackboard.go agent/blackboard_inheritance_test.go` → **차이 없음**.
- `merge-base(main, i18n/korean) == b55ceb1 == main tip` → 사과 대 사과 비교 성립.

→ 번역이 추가한 회귀는 **0건**. 이 실패는 순수 상류 결함이다.

## 6. 영향 범위

- **런타임 영향 없음(기능 회귀 아님)**: `coverage.scope` 제거는 2026-09-24의
  의도된 설계 변경(상황 개요 프롬프트 필드 간소화)이다. planner/worker가 보는 개요에서
  범위 상세를 뺀 것은 기획 의도이며, 범위 정보는 여전히 `list_assets` /
  `list_untested_assets` / `add_task_scope` 등 on-demand 도구로 조회 가능하다.
- **영향은 테스트 스위트 한정**: `go test ./...`가 이 한 건 때문에 red로 뜬다.

## 7. 권고 (이 브랜치에서는 미적용)

이 문서는 **진단 기록**이며, 상류 프로덕션/테스트 동작 변경은 한국어화 범위 밖이라
이 커밋에서 코드 수정은 하지 않는다. 상류에 제안할 수 있는 선택지:

1. **(권장) 테스트를 현재 설계에 맞게 갱신** — `coverage.scope`/`company_scope`/
   `company_keywords` 단언부(대략 `blackboard_inheritance_test.go:85-105`)를 삭제하고,
   남은 `host_count`·company 자산 연계·intent 타깃 유지 단언만 남긴다. 테스트 이름도
   범위 확장이 아닌 "회사 자산 컨텍스트 노출"에 맞게 조정 고려.
2. **필드 복원** — 설계상 planner가 범위 규칙을 개요에서 직접 봐야 한다면
   `06a43f3`이 지운 블록을 되살린다. 단 이는 간소화 의도를 되돌리는 것이라 비권장.
3. **테스트 CI 추가** — `go test ./...`(+Postgres 서비스 컨테이너, `-p 1`)를 수행하는
   GitHub Actions 잡을 두어 이런 stale 테스트가 재발·방치되지 않게 한다.

## 8. 재현 방법

```bash
# Postgres 기동 (예: Docker)
docker run -d --rm --name artex-pg \
  -e POSTGRES_USER=artex -e POSTGRES_PASSWORD=artex -e POSTGRES_DB=artex \
  -p 55432:5432 postgres:16-alpine

export ARTEX_PG_DSN="postgres://artex:artex@localhost:55432/artex?sslmode=disable"

# 실패 재현 (main / i18n/korean 어느 쪽이든 동일)
go test ./agent -run TestGraphOverviewExpandsAssociatedCompanyScope -v
```
