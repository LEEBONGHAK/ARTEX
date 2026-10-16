import assert from "node:assert/strict";
import test from "node:test";
import { activeMention, mentionSearch, mentionToken, selectedMentions } from "./chat-mentions.ts";

test("mention trigger supports CJK input and cursor placement without hijacking email", () => {
  assert.equal(activeMention("user@example.com", 16), null);
  assert.equal(activeMention("선택됨 @[漏洞#1 X]", 13), null);
  assert.deepEqual(activeMention("보기@취약점 뒤의 글자", 6), { start: 2, end: 6, query: "취약점" });
  assert.equal(activeMention("@취약점\n다음 줄", 9), null);
});

test("categories, Korean aliases, IP and keyword search", () => {
  assert.equal(mentionSearch("").categories.length, 9);
  assert.equal(mentionSearch("취").categories[0].kind, "finding");
  assert.equal(mentionSearch("취약점").kind, "finding");
  assert.equal(mentionSearch("취약점SQL인젝션").query, "SQL인젝션");
  assert.equal(mentionSearch("ip 192.0.2.1").kind, "ip");
  assert.equal(mentionSearch("엔드포인트 GET /api").query, "GET /api");
  assert.equal(mentionSearch("acme.com").kind, "");
});

test("tokens keep the stable wire keyword while the display label is localized", () => {
  const first = mentionToken({ kind: "finding", id: 12, label: "제목[1]\n설명" });
  const second = mentionToken({ kind: "ip", id: 13, label: "192.0.2.1" });
  // 저장되는 토큰 키워드는 바뀌지 않는다(백엔드·과거 대화 호환).
  assert.ok(first.startsWith("@[漏洞#12 "));
  const value = `분석 ${first} 와 ${second}`;
  const selected = selectedMentions(value);
  assert.equal(selected.length, 2);
  // 표시 라벨은 한국어로 현지화된다.
  assert.equal(selected[0].label, "취약점 #12 · 제목（1） 설명");
  const next = value.slice(0, selected[0].start) + value.slice(selected[0].start + selected[0].token.length);
  assert.equal(selectedMentions(next)[0].token, second);
});
