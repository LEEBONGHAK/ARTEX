package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo 발행 소스다. 설정 항목으로 만들지 않고 하드코딩한 것은:업데이트 소스를 설정 가능하게 하면 설정을 바꿀 수 있는 누구에게나
// 원격 코드 실행 통로를 주는 셈이라, 침투 테스트 플랫폼에게 이 구멍은 열면 안 된다.
const Repo = "Autumn-27/artex"

// latestURL GitHub의 "최신 정식판" 인터페이스다. prerelease와 draft를 자동으로 건너뛴다.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts 업그레이드 경로가 접근할 수 있는 도메인을 한정한다. 아래 checkRedirect와 함께,
// 어느 한 홉이라도 명단 밖 호스트로 리다이렉트되면 바로 실패한다——이것은 DNS 오염 / 중간자가
// 바이너리를 바꿔치기하는 것을 막는 첫 관문이고, 두 번째는 SHA256SUMS 대조다.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release release 자산이 실제로 떨어지는 객체 스토리지
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release GitHub Release에서 우리가 관심 있는 필드다.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset Release에 달린 파일 하나다.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient GitHub 도메인만 인식하는 HTTP 클라이언트를 구성한다. proxy가 비면 직접 연결.
//
// 의도적으로 기본 Transport를 재사용하지 않는다:업그레이드 경로는 반드시 TLS를 강제하고 인증서를 검증해야 하며, 다른 곳에서
// 설정한 InsecureSkipVerify 같은 것에 영향받으면 안 된다.
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // 전체 패키지 다운로드, 요청 레벨 타임아웃으로 멈추면 안 됨
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("리다이렉트 횟수 과다")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL https + 도메인 화이트리스트를 강제한다.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("비 HTTPS 주소 거부: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("비 GitHub 도메인 거부: %s", u.Hostname())
	}
	return nil
}

// FetchLatest 최신 정식판을 조회한다.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub 접근 실패（시스템 설정에서 전역 프록시를 설정할 수 있음）: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 미인증 GitHub API는 IP당 시간당 60회라, 출구 IP를 공유하면 쉽게 부딪힌다.
		return nil, fmt.Errorf("GitHub 인터페이스 레이트 리밋（시간당 60회）, 잠시 후 다시 시도하세요")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("저장소 %s가 아직 정식 버전을 발행하지 않음", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 반환 %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release 파싱 실패: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release에 tag 누락")
	}
	return &rel, nil
}

// AssetName 현재 플랫폼에 대응하는 발행 패키지명을 반환하며, build.sh의 package_binary와 일치시킨다:
// artex-<버전>-<os>-<arch>.zip（버전 번호는 v 접두사 없음）
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset Release에서 이름으로 자산을 찾는다.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
