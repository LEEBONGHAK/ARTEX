package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout 각각 연결 수립과 SMTP 세션 전체를 제약한다.
// net/smtp 자체에 어떤 타임아웃 메커니즘도 없어, 이 두 가지를 설정하지 않으면, 막힌 대상단이
// 전송 goroutine을 영원히 거기 매달리게 한다——그런데 dispatcher는 단일 goroutine으로 직렬 처리하므로,
// 전체 알림 시스템이 멈추는 셈이다.
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel SMTP 메일 전송을 구현한다.
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// 메일은 플랫폼 레이트 리밋이 없지만, 그것으로 도배해선 안 된다；넉넉한 기본값을 준다.
func (emailChannel) DefaultRatePerMin() int { return 60 }

// 비밀번호만 마스킹한다. SMTP 호스트、계정、수신자는 비밀이 아니며, 그것들을 마스킹하면 편집만 번거로워진다.
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port host/port는 비밀번호를 어느 서버에 줄지 결정한다；tls는 암호화 전송 여부를 결정한다. 셋 중 하나라도 바뀌면
// 비밀번호를 다시 표명하도록 요구한다——겸사겸사「TLS 끄기」이 단계가 반드시 자격 증명을 명시적으로 지니게 하고, 손 가는 대로 바꾸는 게 아니게 한다.
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP 서버 주소 누락")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP 포트 무효（1-65535여야 함）")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("발신자 주소 누락")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("수신자 주소가 최소 하나 필요")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS:대상단이 지원하면 업그레이드. 평문 세션에서는 자격 증명을 보낼 수 없다（아래 auth 설명 참조）.
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS 실패: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth 미암호화 연결에서 자격 증명 발송을 거부한다（대상이 localhost가 아닌 한）.
			// 이것은 **올바른** 보안 동작이고, 우회할 수 없지만, 이유를 명확히 번역해 줄 필요가 있다——
			// 아니면 사용자는「unencrypted connection」만 보고 어떻게 해야 할지 모른다.
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("자격 증명 발송 거부:연결이 암호화되지 않음. TLS를 켜거나, 465 포트(암시적 TLS)로 바꾸거나,「TLS 활성화」를 체크하세요 (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 인증 실패: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("발신자 %s 거부됨", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("수신자 %s 거부됨", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA 실패: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("메일 본문 쓰기 실패: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("메일 제출 실패: %w", err)
	}
	// Quit 실패는「메일이 서버에 수신됨」이라는 사실에 영향을 주지 않으므로, 그 에러를 무시한다.
	_ = client.Quit()
	// 메일은 길이 절단이 없어（HTML 본문 전체 발송）, 전체 배치를 송달로 친다.
	return len(m.Items), nil
}

// emailDial SMTP 연결을 수립한다.
//
// implicitTLS=true implicitTLS=true는 465 같은「연결 즉시 TLS」방식으로 간다；false는 25/587 평문 연결 후 다시
// STARTTLS. 둘을 섞을 수 없다:465 포트에 평문 greeting을 보내면 바로 끊긴다.
//
// 세션 기한은 **연결 수립 지점**에서 설정한다（사후 보충이 아니라）, 왜냐하면 net/smtp의 Client가 하위 계층
// 연결을 미export 필드에 숨겨, 외부가 그것을 얻지 못한다；연결이 일단 넘어가면 미리 설정한 deadline
// 에만 의존해 폴백할 수 있다. 이것은 겸사겸사 핸드셰이크 단계의 블로킹도 커버한다.
// Control 에 blockInternalDial을 달아 HTTP 계열 채널과 같은 가드를 공용한다. 달지 않으면 SMTP
// 가 바로 전체 SSRF 방어의 구멍이 된다:host에 169.254.169.254 또는 127.0.0.1을 넣으면 바로 연결되고,
// 그리고 smtp.NewClient 핸드셰이크 실패 시 대상단이 반환한 그 줄을 에러에 담아、다음을 거침: last_error
// 전송 히스토리 인터페이스가 되비춰, 반맹독 프리미티브를 구성한다；「연결 거부 vs 타임아웃」의 소요 시간 차이는 또
// 포트 탐지에 쓰일 수 있다. 다이얼 단계가 최종 적용 지점이고, DNS 리바인딩도 커버한다.
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP 서버 연결 실패: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP 핸드셰이크 실패: %w", err)
	}
	return client, nil
}

// smtpStageError SMTP 응답 코드에 따라 어떤 단계의 실패를「재시도 가능」과「영구 실패」로 나눈다.
//
// 왜 반드시 구분해야 하나:SMTP의 4xx와 5xx 의미는 완전히 다르다——
//   - 4xx（450 그레이리스트、451 로컬 오류、452 저장 부족）는 **임시** 거부,
//     정규 방식은 나중에 재시도하는 것이다；특히 그레이리스트는, 거의 매번 첫 전송 때 만난다.
//   - 5xx（550 사용자 없음、553 주소 부적법）는 영구 거부, 재시도는 의미 없다.
//
// 일률적으로 영구 실패로 판정하면, 그레이리스트를 켠 메일 서버가 **모든** 푸시를 첫 번째
// 시도 후 failed로 떨어뜨린다——그런데 이런 실패야말로 자동 재시도가 가장 작용해야 할 시나리오다.
// 응답 코드는 에러 텍스트 앞 세 자리 숫자를 취한다；코드를 못 얻으면 재시도 가능으로 처리한다（한 번 더 시도할지언정,
// 파싱 못 했다고 가능한 일시적 장애를 사형 선고하지는 않는다）.
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode SMTP 에러 텍스트에서 선두의 세 자리 응답 코드를 취하고, 못 얻으면 0을 반환한다.
// net/smtp 에러 코드 필드를 export하지 않아, 텍스트에서만 취할 수 있다；형식은「450 4.7.1 ...」이다.
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage 완전한 RFC 5322 메일을 조립한다.
//
// 본문을 base64로 인코딩하는 데는 두 가지 이유가 있다:하나는 SMTP가 단일 행 1000바이트 초과 금지를 규정하는데, HTML
// 본문（특히 요약 메일）은 초장문 행이 쉽게 나타난다；둘은 base64가 본래 "."로 시작하는
// 행이 나타나지 않아, SMTP 점 이스케이프의 번거로움을 던다.
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 중국어 제목은 반드시 RFC 2047 인코딩을 해야 하고, 아니면 클라이언트에 깨진 글자로 표시된다.
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// 메일은 길이 하드 상한이 없어, 본문을 절단하지 않는다.
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 base64는 76자마다 줄바꿈해, RFC 2045에 부합한다.
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
