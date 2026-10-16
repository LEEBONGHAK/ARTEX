package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset release.yml이 생성하는 체크섬 목록으로, Release의 모든 zip을 커버한다.
const sumsAsset = "SHA256SUMS"

// maxBinarySize 압축 해제된 바이너리 크기를 제한해, 기형 zip이 디스크를 꽉 채우는 것을 방지한다.
const maxBinarySize = 512 << 20 // 512 MiB

// Phase 업그레이드 과정의 단계이며, SSE 이벤트의 phase 필드로 직접 쓴다.
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress 호출 측이 제공하며, 진행도를 프런트엔드에 푸시하는 데 쓴다. pct는 다운로드 단계에만 의미 있다（0-100）,
// 나머지 단계는 -1을 전달한다.
type Progress func(ph Phase, pct int, msg string)

// Stage 지정된 Release의 현재 플랫폼 발행 패키지를 다운로드하고, 검증 후 새 바이너리를 artex.new로 스테이징한다.
//
// 완전한 zip을 쓰고 맨 바이너리가 아닌 이유는 두 가지다:기존 Release의 SHA256SUMS가 본래
// zip만 커버하므로, zip을 쓰면 CI를 바꿀 필요 없고, 이미 배포된 과거 버전과도 호환된다；zip 안에는 또
// skills/skills/가 들어 있어, 장래 내장 skill 동기화를 위한 통로를 남겼다. 대가는 skills 그 수백 KB를 더 다운로드하는 것뿐이다.
//
// 함수 반환은 곧 스테이징 완료를 의미하고, 호출 측은 이후 우아하게 종료하고 ExitRestart로 나간다.
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("이 버전은 %s/%s 발행 패키지를 제공하지 않음（%s 누락）", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "체크섬 목록 가져오는 중…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s 에 %s 미수록, 검증되지 않은 바이너리 설치 거부", sumsAsset, name)
	}

	// 임시 파일을 모두 대상 디렉터리에 떨어뜨려, 마지막 rename이 같은 파일 시스템 내 원자적 작업이 되도록 보장한다
	// （크로스 디바이스 rename은 실패하고, /tmp는 흔히 독립 마운트 지점이다）.
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("다운로드 %s（%s）…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 검증 중…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 불일치:기대 %s, 실제 %s（다운로드 손상 또는 변조）", short(want), short(got))
	}

	prog(PhaseExtract, -1, "압축 해제 및 스모크 테스트…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("새 버전이 현재 시스템에서 실행 불가: %w", err)
	}

	// 스테이징 파일 자체의 sha256을 따로 한 부 저장한다:다음 시작 교체 전에 한 번 더 검증해야 하므로,
	// 스테이징 후 재시작 전 이 시간 동안 파일이 변경되거나 손상되는 것을 방지한다.
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("새 바이너리 체크섬 계산: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("체크섬 쓰기: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("새 버전 스테이징: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// 마커는 자동 롤백 능력에만 영향을 주고, 스테이징 파일 자체는 이미 제자리이니, 이 때문에 업그레이드를 중단하지 않는다.
		prog(PhaseStaged, -1, "경고:업그레이드 마커 쓰기 실패, 이번 업그레이드는 자동 롤백 보호가 없습니다")
	}

	prog(PhaseStaged, 100, "새 버전 준비 완료, 재시작 중…")
	return nil
}

// fetchSums SHA256SUMS를 다운로드해 파싱하고, 파일명 → 16진 다이제스트를 반환한다.
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("이 Release에 %s 없음, 무결성 검증 불가, 업그레이드 거부", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("다운로드 %s: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("읽기 %s: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s 내용이 비었거나 형식을 인식할 수 없음", sumsAsset)
	}
	return out, nil
}

// parseSums sha256sum 스타일의 목록을 파싱하고, 파일명 → 16진 다이제스트를 반환한다.
//
// 첫 번째 필드가 반드시 64자리 16진수여야 수록한다. "정확히 두 필드"로만 판단하는 것은 부족하다——
// 두 단어로 된 아무 설명 텍스트 행도 합법 항목으로 간주돼, 쓰레기 값을 다이제스트 표에 쑤셔 넣어,
// 진짜 자산이 오히려 잘못된 다이제스트에 매칭될 수 있다.
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 형식은 "<sha256>  <filename>"（sha256sum은 이중 공백 사용；shasum의 바이너리
		// 모드는 파일명에 * 접두사를 붙임）.
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download 자산을 dst에 쓰고, 동시에 SHA256을 계산하며 Content-Length에 따라 진행도를 보고한다.
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("다운로드 %s: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("임시 파일 생성: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("다운로드 중단: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("디스크 기록 실패: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("다운로드 불완전:기대 %d바이트, 실제 %d바이트", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get 화이트리스트 제약을 받는 GET을 발행하고, 응답 본문을 반환한다.
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary 발행 패키지에서 artex 실행 파일을 꺼낸다.
//
// 패키지 내 구조는 artex-<버전>-<os>-<arch>/artex이지만, 여기서는 **기본명**으로 매칭하고 완전한 경로를 조합하지 않는다
// 경로:버전 번호가 패키지명에 한 번 나타나는데, 한 글자만 틀려도 전체 업그레이드가 실패하니, 기본명으로 찾는 게 변경에 더 강하다.
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("발행 패키지 열기: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("읽기 %s: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("새 바이너리 쓰기: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("압축 해제 %s: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("발행 패키지 내 실행 파일이 %s를 초과, 압축 해제 거부", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("발행 패키지 내 %s는 빈 파일", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("발행 패키지에서 찾지 못함: %s", want)
}

// checkWritable 디렉터리 쓰기 가능을 미리 확인한다. 이 단계가 없으면, 비 root 실행、또는 바이너리가 시스템
// 디렉터리에 놓일 때, 수십 MB를 다 다운로드한 뒤에야 교체하는 그 순간에 실패한다.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("프로그램 디렉터리 %s 쓰기 불가, 자동 업데이트 불가（권한을 확인하거나 수동 업그레이드로 전환하세요）: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter 쓴 바이트를 집계하고 빈도를 제한해 보고하여, 매 32KiB 청크마다 SSE를 하나씩 푸시하는 것을 피한다.
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("다운로드 중 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
