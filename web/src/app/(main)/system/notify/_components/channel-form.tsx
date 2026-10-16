"use client";

import { CheckIcon } from "lucide-react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { NotificationFilter } from "@/lib/types";

// asText / inputType 는 이 파일 내 값 추출 보조(컨트롤 렌더링과 밀접), 다음에 두지 않음: channel-fields.
import { type FieldDef, type FieldKind, SEVERITY_OPTIONS } from "./channel-fields";

// asText 임의의 설정 값을 입력란에서 쓸 수 있는 문자열로 렌더링.
// config 출처 JSON, 값은 다음일 수 있음: string / number / boolean / array / null,
// 여기서는 「텍스트 상자에 넣을 수 있는지」만 신경 씀, 구체적 직렬화는 buildConfig 담당.
function asText(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === null || v === undefined) return "";
  return String(v);
}

// inputType 필드 유형을 다음에 매핑: input 의 type 속성.
function inputType(kind: FieldKind): "text" | "password" | "number" {
  if (kind === "password") return "password";
  if (kind === "number") return "number";
  return "text";
}

// ConfigField 필드 정의에 따라 해당 컨트롤을 렌더링.
//
// 마스킹 필드 처리가 여기서 유일하게 신경 쓸 점: 입력란**표시 안 함**마스킹 값 자체, 한 줄만 표시
// 「저장됨」 안내. 이렇게 하면 화면에 규칙이 하나만——상자에 글자가 있으면 사용자가 입력한 것,
// 빈 상자는 빈 값. 만약 "__masked__:…abc123" 입력란에 넣으면 사용자가 그것을 직접 입력해야 한다고 오해함
// 삭제된 placeholder 텍스트로, 오히려 자격 증명을 실수로 지우기 쉬움.
export function ConfigField({
  def,
  value,
  isSecret,
  onChange,
}: {
  def: FieldDef;
  value: unknown;
  isSecret: boolean;
  onChange: (v: unknown) => void;
}) {
  const id = `n-cfg-${def.key}`;
  const raw = asText(value);
  // 백엔드가 되돌려준 마스킹 값: 다음 형식: "__masked__:…abc123", 끝부분은 원래 값의 식별 가능한 조각.
  const masked = isSecret && raw.startsWith("__masked__");
  const maskedTail = masked ? (raw.split("…")[1] ?? "") : "";

  if (def.kind === "switch") {
    return (
      <div className="flex items-center gap-2 text-sm">
        <Switch checked={value === true} onCheckedChange={onChange} aria-label={def.label} />
        {def.label}
        {def.help && <span className="text-muted-foreground">({def.help})</span>}
      </div>
    );
  }

  if (def.kind === "select") {
    return (
      <div className="grid gap-2">
        <Label>{def.label}</Label>
        <Select value={raw || def.options?.[0]?.value} onValueChange={onChange}>
          <SelectTrigger>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {(def.options ?? []).map((o) => (
              <SelectItem key={o.value} value={o.value}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    );
  }

  // 컨트롤은 필드 유형별로 분배. 다음 사용: if 중첩 삼항이 아니라 체인인 이유는 여기서 네 종류 컨트롤을 구분해야 하기 때문,
  // 3단계 삼항은 읽을 때 이미 멈춰서 괄호를 세야 할 지경.
  function control() {
    if (def.kind === "textarea" || def.kind === "kv") {
      return (
        <Textarea
          id={id}
          className="font-mono"
          placeholder={def.placeholder}
          value={masked ? "" : raw}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    }
    if (def.kind === "list") {
      return (
        <Input
          id={id}
          value={Array.isArray(value) ? (value as string[]).join(", ") : raw}
          onChange={(e) => onChange(e.target.value)}
          placeholder={def.placeholder}
        />
      );
    }
    return (
      <Input
        id={id}
        className={def.kind === "text" ? "font-mono" : ""}
        type={inputType(def.kind)}
        placeholder={def.placeholder}
        value={masked ? "" : raw}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }

  const hint = masked ? (
    <p className="text-muted-foreground flex items-center gap-1 text-xs">
      <CheckIcon className="size-3" />
      저장됨{maskedTail ? `(끝번호 ${maskedTail})` : ""} · 새 값을 입력하면 덮어쓰고, 비우면 해당 항목 삭제
    </p>
  ) : (
    def.help && <p className="text-muted-foreground text-xs">{def.help}</p>
  );

  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{def.label}</Label>
      {control()}
      {hint}
    </div>
  );
}

// FilterSummary 필터 조건을 한 줄로 요약하여, 카드를 펼치지 않아도 이 채널이 무엇을 푸시하는지 알 수 있게 함.
export function FilterSummary({ filter }: { filter: NotificationFilter }) {
  const parts: string[] = [];
  if (filter.min_severity) {
    parts.push(SEVERITY_OPTIONS.find((o) => o.value === filter.min_severity)?.label ?? filter.min_severity);
  }
  if (filter.vulnclass_include?.length) parts.push(`유형 포함 ${filter.vulnclass_include.length} 단어`);
  if (filter.vulnclass_exclude?.length) parts.push(`제외 ${filter.vulnclass_exclude.length} 단어`);
  if (filter.task_ids?.length) parts.push(`${filter.task_ids.length} 개 작업`);
  if (filter.asset_ids?.length) parts.push(`${filter.asset_ids.length} 개 자산`);
  if (filter.on_status_change) parts.push("상태 변경 포함");
  if (parts.length === 0) {
    return <p className="text-muted-foreground text-sm">전체 취약점</p>;
  }
  return <p className="text-muted-foreground text-sm">{parts.join(" · ")}</p>;
}
