// 채널 필드 표와 설정 값의 파싱 도구.
//
// 페이지와 분리한 이유는 이것이**데이터**뷰가 아니라: 각 채널에 어떤 필드가 있는지 설명하며,
// 각각 어떤 컨트롤을 쓸지, 그리고 폼 텍스트에서 설정 값으로(JSON)의 양방향 변환.
// 별도 파일로 두면 채널 추가 시 여기만 수정하면 되고 페이지 자체는 바꿀 필요가 없습니다.
// 채널 유형의 표시 이름과 소개. 프런트엔드에 둔 이유는 문구에만 영향을 주고 백엔드는 알 필요가 없기 때문입니다.
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "딩톡",
  feishu: "페이슈",
  wecom: "기업 위챗",
  webhook: "범용 Webhook",
  telegram: "Telegram",
  email: "이메일",
};

// 각 채널의 설정 필드 정의.
//
// 여기서는 의도적으로 프런트엔드 필드 표를 유지하며, 백엔드가 내려보내게 하지 않습니다 schema: 백엔드는 다음만 담당:
// Validate(필수/형식),UI 필요한 것은 레이아웃과 컨트롤 유형이며, 둘은 같은 것을 다루지 않습니다.
// 유일한 결합점은 secret_keys —— 어떤 필드를 비밀번호 입력으로 렌더링할지는 백엔드가 제공하며,
// 어떤 값이 자격 증명인지는 채널 구현만 알기 때문입니다(기업 위챗의 전체 Webhook 는 곧 자격 증명,
// 반면 딩톡은 그중 하나일 뿐 secret). 채널 추가 시 여기에 항목이 하나 빠지면 폼이 비어 보일 뿐,
// 무음으로 오류 나지 않음(아래의 hasFields 안내함).
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "서명 추가 키",
      kind: "password",
      help: "봇 보안 설정에서 「서명 추가」를 선택할 때 입력; 「사용자 지정 키워드」를 선택했거나 보안 설정을 켜지 않았으면 비움",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    { key: "secret", label: "서명 검증 키", kind: "password", help: "봇이 「서명 검증」을 켤 때 입력, 그러지 않으면 비움" },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook 주소",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "대상 URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "요청 메서드",
      kind: "select",
      options: [
        { value: "POST", label: "POST(요청 본문 포함)" },
        { value: "PUT", label: "PUT(요청 본문 포함)" },
        { value: "PATCH", label: "PATCH(요청 본문 포함)" },
        { value: "GET", label: "GET(요청 본문 없음)" },
      ],
    },
    { key: "headers", label: "사용자 지정 요청 헤더", kind: "kv", help: "각 줄 KEY=VALUE, 예: Authorization=Bearer xxx" },
    {
      key: "body_template",
      label: "요청 본문 템플릿",
      kind: "textarea",
      help:
        "비우면 내장 기본 템플릿 사용. 변수:{{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}," +
        "그리고 range .Items 아래의 .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel." +
        "문자열 삽입은 다음 사용: {{json .Xxx}} 가 아니라 {{.Xxx}}, 그러지 않으면 제목의 따옴표가 깨뜨림 JSON.",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API 주소",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "비우면 공식 주소 사용; 자체 구축 Bot API 리버스 프록시 시 입력",
    },
  ],
  email: [
    { key: "host", label: "SMTP 서버", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "포트",
      kind: "number",
      placeholder: "587",
      help: "587 경유 STARTTLS;465 「암시적 TLS」 열기",
    },
    { key: "username", label: "계정", kind: "text" },
    { key: "password", label: "비밀번호 / 인증 코드", kind: "password" },
    { key: "from", label: "발신자", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "수신자", kind: "list", help: "여러 주소는 쉼표로 구분" },
    { key: "tls", label: "암시적 TLS", kind: "switch", help: "465 포트 열림;587 꺼짐 유지(자동으로 STARTTLS)" },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "무제한" },
  { value: "low", label: "낮음 이상" },
  { value: "medium", label: "중간 이상" },
  { value: "high", label: "높음 이상" },
  { value: "critical", label: "심각만" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV 「한 줄에 하나」 파싱 KEY=VALUE」 텍스트 영역.
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs 쉼표 파싱/공백으로 구분된 id 목록.
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords 행 파싱/쉼표로 구분한 키워드 목록(취약점 유형명에 공백이 있을 수 있어 줄 또는 쉼표로 분리).
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
