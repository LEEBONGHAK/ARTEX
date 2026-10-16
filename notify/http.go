package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets 메시지를 루프백 / 링크 로컬 주소로 전송하는 것을 허용할지 결정한다.
//
// 기본은 거부. 이 몇 개 주소 대역은 IM 봇이나 공용망 메일 서버가 나타나는 곳이 아니며, 그것들이
// 칠 수 있는 대상은 매우 민감하다:같은 머신의 다른 서비스 관리 포트、그리고 클라우드 환경의 메타데이터 엔드포인트
// （169.254.169.254, 인스턴스 자격 증명을 읽어낼 수 있음）. 전송 주소는 관리자가 설정하지만, XSS/CSRF에 XSS/CSRF
// 탈취당한 관리 세션、또는 같은 JWT를 공유하는 두 번째 사람도, 설정을 바꿔 응답 내용을 읽어낼 수 있다
// ——doJSON 4xx/5xx 응답 본문 앞 200바이트를 last_error에 쓰고, 전송 히스토리 인터페이스가
// 그것을 되비쳐 보여주니, 이것이 바로 하나의 반맹독(half-blind read) 프리미티브다.
//
// 하지만「로컬 SMTP 릴레이」（127.0.0.1:25의 postfix）는 자체 구축 메일의 흔한 설정이라,
// 일괄 차단은 사람을 막히게 한다. 그래서 하드코딩으로 허용하는 대신 명시적 탈출구 하나를 남긴다:
// ARTEX_NOTIFY_ALLOW_LOCAL=1 설정 시 허용된다.
//
// AllowLocalTargetsEnv로 익스포트한 것은 테스트가 명시적으로 켤 수 있게 하기 위함이다——이 패키지와 server 패키지의
// 테스트 케이스가 127.0.0.1의 httptest 가짜 수신단을 대량으로 쓰는데, 켜지 않으면 전부 가드에 막힌다.
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP 대상 IP가「기본적으로 전송 불허」주소 대역에 속하는지 보고한다.
//
// 루프백、링크 로컬（클라우드 메타데이터 169.254.169.254 포함）、미지정 및 멀티캐스트만 거부한다.
// **거부하지 않는다** RFC1918 사설망:내부망 자체 구축 Mattermost / SMTP 릴레이는 매우 흔한 합법적 용법이라,
// 그것들까지 함께 막으면 기능이 실제 환경에서 바로 쓸 수 없게 된다. 이 절충은 의도적이다——
// 보호는 진짜 민감한 대상을 막으면서, 동시에 정상 배포를 함께 못 쓰게 만들어선 안 된다.
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6（::ffff:127.0.0.1）은 IPv4로 복원한 뒤 판정해야 하고, 아니면 검사를 우회한다.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial http.Transport 다이얼러의 Control 훅으로, **연결 수립 시**
// 대상 주소를 검사한다.
//
// 왜 설정 저장 시에만 검증하지 않고 다이얼 단계에 두는가:여기가 바로 최종 적용 지점이기 때문이다.
// 이것은 설정 검증을 우회하는 두 가지 상황을 동시에 커버한다——DNS 리바인딩（검증 시 공용망 IP로 해석、
// 실제 연결 시 내부망으로 해석）과 리다이렉트（우리가 이미 크로스 호스트 점프를 거부했지만, 같은 호스트 점프는
// 여전히 경로를 다른 곳으로 가리킬 수 있다）.
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("대상 주소를 해석할 수 없음 %q %q", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("로컬 머신/링크 로컬 주소 %s로의 전송 거부（로컬 머신 서비스로 전송이 꼭 필요하면 %s=1 설정）", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport 기본 Transport를 바탕으로 다이얼 가드 하나만 추가한다.
// Clone으로 기본값의 모든 튜닝을 보존한다（커넥션 풀、HTTP/2、타임아웃、proxy 등）,
// 검사 하나를 추가하려고 다른 동작을 바꾸는 것을 피한다.
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient 모든 채널 전송이 공용으로 쓰는 클라이언트다.
//
// 의도적으로 프로젝트의 전역 출구 프록시（server 측 GlobalProxy）를 **재사용하지 않는다**:그 프록시는 침투
// 대상 트래픽용이라, 흔히 불안정한 터널이고, 알림의 가용성이 대상 네트워크의 흔들림에 인질로 잡혀선 안 된다.
// IM IM 푸시는 직접 연결하면 된다. 타임아웃은 15초로 설정——이보다 느린 대상은 사실상 이미 장애 상태다.
//
// 크로스 호스트 리다이렉트 거부:이 기능의 전송 주소는 모두「고정 endpoint 하나」형태라, 정상적으론
// 다른 호스트로 리다이렉트하지 않는다;그런데 이들의 자격 증명（딩톡의 access_token、위챗워크의 key、Telegram의
// bot token）**이 바로 URL 안에 있어**, 크로스 호스트 점프를 따라가면 자격 증명을 리다이렉트 대상에 넘기는 셈이다. 같은 호스트
// 의 점프（예: 끝에 슬래시 보충）는 여전히 허용한다.
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("리다이렉트 횟수 과다")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("크로스 호스트 리다이렉트 거부（%s → %s）", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit 응답 본문을 읽는 크기를 제한한다. 대상단이 이상할 때 초대형 내용을 뱉어낼 수 있는데, 우리는
// 에러 코드와 짧은 에러 설명만 전송 히스토리에 표시하는 데 필요하다.
const respBodyLimit = 8 << 10

// doJSON 요청을 한 번 보내고 응답 본문을 반환한다（길이 제한됨）.
//
// payload payload가 nil일 때 빈 body를 보낸다（GET 또는 플랫폼이 body를 요구하지 않는 경우에 쓴다）.
// headers 안의 키-값을 원본 그대로 덧붙이며, 범용 Webhook의 커스텀 헤더에 쓴다.
//
// 에러 분류는 이 함수의 핵심 책무다:네트워크 계층 실패와 5xx/408/429는「재시도 가능」으로 분류하고,
// 나머지 4xx는「영구 실패」로 분류한다——403을 재시도해봐야 같은 에러로 로그를 3번 도배할 뿐이다.
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// 직렬화 실패는 로컬 버그다（설정 필드 타입이 틀림）, 재시도해도 나아지지 않는다.
			return nil, Permanent(fmt.Errorf("요청 본문 구성 실패: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL URL이 부적법——대개 사용자가 주소를 잘못 입력한 것이고, 영구 실패에 속한다.
		// 여기서도 err를 그대로 전달할 수 없다:url.Parse의 에러 텍스트에는 전체 주소가 들어 있다.
		return nil, Permanent(fmt.Errorf("요청 주소가 부적법함: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 연결 거부、DNS 실패、타임아웃——대부분 일시적 장애이니, 백오프 재시도에 맡긴다.
		//
		// 에러 텍스트는 반드시 마스킹한 뒤 밖으로 전달해야 한다. 이유:http.Client.Do가 반환하는 것은 *url.Error이고,
		// 그것의 Error()는 `Op "전체URL": 하위 계층 에러`이고, 이 기능에서 이들의 자격 증명은 **바로 URL 안에 있다**
		// （딩톡 access_token、위챗워크 key、페이슈 hook id、Telegram /bot<token>/）.
		// 마스킹하지 않으면, 자격 증명이 이 에러 문자열을 따라 네 곳으로 흘러간다:notification_deliveries
		// 의 last_error（평문 저장）、전송 히스토리 인터페이스의 응답（**채널 설정의 마스킹을 우회**）、
		// 서버 측 로그、그리고 테스트 발송 인터페이스가 프런트엔드에 돌려주는 502 텍스트.
		return nil, fmt.Errorf("요청 실패: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("응답 읽기 실패: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429（레이트 리밋）와 408（타임아웃）는 재시도할 가치가 있다;나머지 4xx는 설정이나 권한 문제라 재시도는 무의미하다.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("상대방 레이트 리밋 또는 타임아웃 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("상대방 서비스 이상 (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("상대방이 요청 거부 (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet 응답 본문을 한 줄 짧은 텍스트로 압축해, 에러 정보에 쓴다. 응답에는 줄바꿈과 대량의 공백이 있을 수 있어,
// 그대로 last_error에 쑤셔 넣으면 전송 히스토리 페이지의 레이아웃이 깨진다.
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget 전송 주소를「scheme://host/…」로 압축해, 에러 정보에 쓴다.
//
// 이것은 이 패키지 유일의 주소 마스킹 기준이며, 의도적으로 **충분히 거칠게** 만들었다:scheme과 host를 빼면,
// 나머지는 일률적으로 버린다. 이유는 URL의 어느 부분이 자격 증명인지 판단할「범용적이고 안전한」방법이 하나도 없기 때문이다:
//
//	딩톡   자격 증명은 query      /robot/send?access_token=xxx
//	위챗워크   자격 증명은 query      /cgi-bin/webhook/send?key=xxx
//	페이슈   자격 증명은 **경로 끝 구간** /open-apis/bot/v2/hook/<hook_id>
//	Telegram 자격 증명은 **경로 중간 구간** /bot<token>/sendMessage
//
// 「유용한 부분만 남기기」를 원하면 채널별로 패치해야 하는데, 어느 하나라도 빠뜨리면 그게 바로 한 번의 자격 증명 유출이다.
// host만 남겨도 트러블슈팅에는 충분하다（DNS 해석 불가、연결 불가、인증서 불일치 모두 특정 가능）,
// 구체적으로 어느 봇인지는 채널 설정의 마스킹 끝자리 힌트로 식별한다.
//
// 해석 실패 시 고정 플레이스홀더를 반환한다——원본 문자열을 절대 되비쳐 보여주지 않는다.
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(주소 해석 불가)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError 전송 계층 에러에서 주소를 벗겨내고, 하위 계층 원인만 남긴다.
//
// *url.Error 의 구조는 {Op, URL, Err}이고, Error()는 URL을 함께 출력한다.
// 여기서 Err 필드를 명시적으로 취해, 그 Error()를 우회한다——사후에 문자열 치환을 하는 것보다 더 믿을 만하다,
// 왜냐하면 치환은 URL 인코딩/이스케이프된 각종 변형에 정확히 대응해야 해서, 빠뜨리기 쉽기 때문이다.
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 알 수 없는 오류", uerr.Op, host)
	}
	// *url.Error가 아닌 것（예: 리다이렉트 정책이 반환하는 에러）도 주소를 가질 수 있으니, 일괄적으로 마스킹을 거친다.
	return redactURLsInText(err.Error())
}

// redactURLsInText 텍스트 한 조각에 나타나는 http(s) 주소를 마스킹 형태로 치환한다.
//
// 구조화 필드를 얻지 못하는 에러의 폴백에 쓴다（리다이렉트 정책 에러、서드파티 라이브러리의 커스텀 에러）.
// http/https 접두사만 인식하고, 공백과 따옴표로 분할한다——주소는 이 두 종류 문자를 포함하지 않는다.
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
