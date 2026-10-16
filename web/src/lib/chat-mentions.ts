// token: 저장되는 와이어 포맷 키워드. `@[<token>#<id> <name>]` 토큰의 일부로 쓰이며
//        백엔드(server/chat_mentions.go)와 과거에 저장된 대화가 이 값에 의존하므로 바꾸지 않는다.
// label: 화면 표시용 한국어 라벨(드롭다운·칩·검색). 자유롭게 바꿔도 저장 데이터와 무관하다.
// alias: 영문 검색 별칭.
export const mentionKinds = [
  { kind: "finding", token: "漏洞", label: "취약점", alias: "finding" },
  { kind: "asset", token: "资产", label: "자산", alias: "asset" },
  { kind: "company", token: "企业", label: "기업", alias: "company" },
  { kind: "endpoint", token: "接口", label: "엔드포인트", alias: "api" },
  { kind: "ip", token: "IP", label: "IP", alias: "ip" },
  { kind: "app", token: "应用", label: "애플리케이션", alias: "app" },
  { kind: "root_domain", token: "域名", label: "도메인", alias: "domain" },
  { kind: "subdomain", token: "子域名", label: "서브도메인", alias: "subdomain" },
  { kind: "service", token: "服务", label: "서비스", alias: "service" },
] as const;

// 토큰 키워드 → 표시 라벨. selectedMentions가 저장된 토큰을 한국어로 표시할 때 사용한다.
const tokenToLabel = new Map<string, string>(mentionKinds.map((item) => [item.token, item.label]));

export type MentionKind = (typeof mentionKinds)[number]["kind"];
export interface ChatMention {
  kind: MentionKind;
  id: number;
  label: string;
  description: string;
}

export function activeMention(value: string, caret: number) {
  const before = value.slice(0, caret);
  const start = before.lastIndexOf("@");
  if (start < 0 || (start > 0 && /[\w.+/-]/.test(before[start - 1]))) return null;
  const query = before.slice(start + 1);
  if (/[[\]\r\n@]/.test(query) || query.length > 220) return null;
  return { start, end: caret, query };
}

export function mentionSearch(query: string) {
  const text = query.trimStart().toLowerCase();
  for (const item of mentionKinds) {
    for (const alias of [item.label.toLowerCase(), item.alias]) {
      if (text === alias || text.startsWith(`${alias} `) || (/[^a-z]/.test(alias) && text.startsWith(alias))) {
        return { kind: item.kind, query: query.trimStart().slice(alias.length).trim(), categories: [] };
      }
    }
  }
  const categories = mentionKinds.filter(
    (item) => item.label.toLowerCase().startsWith(text) || item.alias.startsWith(text),
  );
  return { kind: "" as const, query: query.trim(), categories };
}

export function mentionToken(item: ChatMention) {
  const token = mentionKinds.find((entry) => entry.kind === item.kind)?.token ?? "资产";
  const label = item.label
    .replace(/[[\]]/g, (char) => (char === "[" ? "（" : "）"))
    .replace(/\s+/g, " ")
    .slice(0, 100);
  return `@[${token}#${item.id} ${label}]`;
}

export function selectedMentions(value: string) {
  return [...value.matchAll(/@\[(漏洞|资产|企业|接口|IP|应用|域名|子域名|服务)#([0-9]+)(?: ([^\]\r\n]*))?\]/g)].map(
    (match) => ({
      token: match[0],
      label: `${tokenToLabel.get(match[1]) ?? match[1]} #${match[2]}${match[3] ? ` · ${match[3]}` : ""}`,
      start: match.index,
    }),
  );
}
