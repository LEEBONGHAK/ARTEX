// Package selfupdate implements ARTEX 의 페이지 원클릭 업데이트를 구현한다:GitHub Release에서 새 버전
// 바이너리를 가져와、검증、스테이징하고, 다음 시작 시 원자적으로 교체한다.
//
// 전체 분담（start.sh / start.bat 참조）:
//
//	시작 스크립트  = 단순 데몬 루프, "프로세스 종료 후 종료 코드에 따라 다시 띄울지 결정"
//	이 패키지      = 모든 오류 나기 쉬운 로직（다운로드 / SHA256 검증 / 스모크 / 교체 / 실패 롤백）
//
// 교체를 스크립트가 아니라 Go에 둔 이유는, sha256 검증과 스모크 테스트를 sh와 bat에서
// 두 세트 써야 하는데（sha256sum / shasum / certutil）, 이것이 바로 가장 틀리면 안 되는 부분이다——실행 안 되는
// 바이너리로 교체하면, 데몬 프로세스가 충실히 그것을 반복해서 띄우고, 사용자는 머신에 올라가 수동으로 구조할 수밖에 없다.
//
// 한 번의 완전한 업그레이드는 세 번의 프로세스 시작을 거친다:
//
//	① 구버전 server가 /api/update/apply 수신 → 다운로드 검증 → 스테이징 artex.new → exit 75
//	② 스크립트가 구버전을 다시 띄움 → Bootstrap이 artex.new 발견 → 검증+스모크 → 교체 → exit 75
//	③ 스크립트가 다시 띄움, 이때 이미 새 버전 → Bootstrap이 시도 한 번 기록 → 시작 성공 후 마커 제거
//
// 어느 단계든 실패하면 구버전으로 되돌린다:② 검증 못 하면 스테이징 파일을 지우고 구버전 계속 실행；③ 연속 3번 다음까지 못 살아남으면
// 마커 제거（시작 못 하면 크래시）, 그러면 자동으로 artex.old를 되돌린다.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart "데몬 프로세스가 나를 다시 띄워 달라"는 종료 코드다（EX_TEMPFAIL）. 시작 스크립트가 이것을 보면
// 즉시 다시 실행하고, 크래시 백오프에 산입하지 않는다. 0은 사용자 정상 정지를 나타낸다（스크립트가 루프 종료）, 나머지는 모두 크래시로 간주한다.
const ExitRestart = 75

// maxAttempts 교체 후 허용되는 시작 시도 횟수다. 새 버전은 매 시작 시 카운트를 +1하고, 다음까지 살아남으면
// settleDelay settleDelay면 마커를 제거한다；연속 maxAttempts번 크래시하면 새 버전이 아예 시작 못 함을 의미하니, 자동 롤백한다.
const maxAttempts = 3

// Paths 한 번의 업그레이드에 관련된 모든 파일이며, **실행 파일이 있는 디렉터리** 아래에 통일해 둔다.
// 의도적으로 CWD를 쓰지 않는다:서비스화 실행 시 작업 디렉터리가 / 또는 임의 경로일 수 있어, CWD를 쓰면 스테이징 파일이
// 다른 곳에 떨어져, 교체 로직이 바로 무효화된다.
type Paths struct {
	Dir     string // 실행 파일이 있는 디렉터리
	Current string // 현재 실행 중인 바이너리        artex      / artex.exe
	New     string // 스테이징된 새 버전            artex.new  / artex.new.exe
	Sum     string // 새 버전의 sha256（hex）  artex.new.sha256 / artex.new.exe.sha256
	Old     string // 교체 전 백업한 구버전      artex.old  / artex.old.exe
	Marker  string // 업그레이드 상태 마커            artex.upgrade.json
}

// ResolvePaths 현재 실행 파일을 기준으로 모든 업그레이드 경로를 유도한다.
//
// Windows 에서 .new/.old도 반드시 .exe 접미사를 지녀야 하고, 아니면 스모크 테스트와 교체 후 실행이 모두 실패하니,
// 그래서 먼저 접미사를 떼고 다시 조합해야, 두 플랫폼의 명명이 대칭이 된다.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("실행 파일 위치 확인: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows 에서는 ".exe", Unix에서는 보통 비어 있음
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker 교체 한 번의 진행도를 기록하며, 새 버전이 시작 못 할 때 자동 롤백을 트리거하는 데 쓴다.
type marker struct {
	From     string `json:"from"`     // 업그레이드 전 버전
	To       string `json:"to"`       // 목표 버전
	Attempts int    `json:"attempts"` // 교체 후 시작 시도한 횟수
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged 스테이징 파일을 지운다. 교체 성공、검증 실패、사용자 취소 모두 이것을 거쳐, 잔류하는
// artex.new artex.new가 다음 시작 시 다시 시도되는 것을 피한다.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions 두 버전 번호를 비교하고, -1/0/1을 반환한다（a<b / a==b / a>b）.
// ok=false 최소 한쪽이 비교 가능한 버전 번호가 아님을 나타낸다（예: 로컬 개발 빌드의 "dev" 또는
// git describe git describe가 산출한 "0.3.7-2-gabc1234-dirty"）, 이때 호출 측은 원클릭 업데이트를 비활성화해야 하고,
// 아니면 개발 중인 빌드를 정식판으로 "업그레이드"해、커밋되지 않은 변경을 덮어쓴다.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion "v0.3.7" / "0.3.7" 형식의 버전 번호를 [3]int로 파싱한다.
//
// 순수한 3단식만 받는다:build.sh가 non-tag 빌드 시 git describe로 산출하는
// "0.3.7-2-gabc1234" "0.3.7-2-gabc1234" 같은 접미사 붙은 버전은, 반드시 비교 불가로 판정돼야 하고, 다음으로 간주되는 게 아니다
// 0.3.7 —— 아니면 개발 빌드가 "이미 최신"으로 오판되거나 정식판에 덮어쓰인다.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker 프로세스가 컨테이너에서 도는지 보고한다. Docker에서 교체는 컨테이너 쓰기 가능 레이어에 쓰고,
// `docker compose up -d` 컨테이너를 재구축하면 이미지에 내장된 버전으로 되돌아간다——이것은 예상된 동작이다
// （그때 사용자는 원래 새 이미지를 가져오는 중）, 하지만 프런트엔드는 이것에 따라 말을 명확히 할 수 있어야 한다.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
