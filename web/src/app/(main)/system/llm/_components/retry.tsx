"use client";

// LLM 재시도 설정의 공용 컴포넌트: 다섯 계층 재시도 각각의 「횟수 + 간격」.
//
// 다섯 계층, 안쪽에서 바깥쪽으로: 연결 수립(SDK) → 빈 응답(SDK) → 동일 provider 안전 윈도우 → 폴링 차단 → 의도 재실행.
// 앞 세 계층은 엔드포인트를 따르므로 각 모델 설정이 전역 기본값을 재정의할 수 있습니다; 뒤 두 계층은 프로세스 레벨이라 전역에 하나만 있습니다.
//
// 모든 입력은 동일한 「비우면 = 미설정」 의미로, 백엔드와 db.RetryRule 일치:
//   횟수   빈값/0 = 내장 기본값 사용 | -1 = 이 계층 재시도 끄기 | >0 = 이 횟수 사용
//   간격   빈값/0 = 이 계층 본래의 지수 백오프 사용 | >0 = 이 고정 밀리초 간격으로 변경

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** 이 계층 재시도가 어디서, 누가 실행하는지 */
  where: string;
  /** 어떤 오류가 이 계층으로 오는지——상태 코드까지 구체적으로, 추측하지 않게 */
  trigger: string;
  /** 비슷해 보이지만 이 계층을 【거치지 않는】 오류로, 입력해도 반응이 없을 때 오해하지 않도록 bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 횟수를 비웠을 때의 기본값으로, placeholder에 사용 */
  defAttempts: number;
  /** 간격을 비웠을 때의 기본 정책으로, placeholder에 사용 */
  defInterval: string;
  /** 횟수 입력 -1 의 의미 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "연결 수립 재시도",
    where: "SDK · 받음 200 이전",
    trigger:
      "연결 안 됨 또는 아직 받지 못함 200: 연결 리셋 / 읽기·쓰기 타임아웃 / DNS 실패 등 네트워크 계층 오류, 그리고 HTTP 408,429,500,502,503,504.",
    skips: "그 외 상태 코드(400 / 401 / 403 / 404 / 413 / 422 등)은 모두 결정적 거부이므로 재전송해도 똑같이 실패하며 그대로 상위로 던집니다.",
    desc: "같은 요청을 그대로 재전송합니다. 스트림이 일단 시작되면(이미 받음 200), 중간에 끊기면 이 계층 소관이 아닙니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s 지수(상한 8s)",
    offHint: "-1 = 한 번도 재시도하지 않고 실패 시 즉시 상위로 던짐",
  },
  empty: {
    title: "빈 응답 재시도",
    where: "SDK · 단 openai 형식",
    trigger:
      "HTTP 200,finish_reason 은 정상 stop, 그러나 전체 응답에 내용 블록이 하나도 없음——게이트웨이 빈 프레임, 사고 필드 프레임 손실, 샘플링 끊김이 모두 이런 모습입니다.",
    skips: "때문에 max_tokens 잘려서 내용이 없는 경우는 제외(그건 출력 상한을 높여 해결해야 하며, 재전송하면 다시 부딪힐 뿐).",
    desc: "전체 재전송 prompt, 따라서 긴 컨텍스트에서는 비싸므로 횟수를 크게 주지 않는 것이 좋습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s 지수(상한 8s)",
    offHint: "-1 = 빈 응답을 그대로 전달",
  },
  stream: {
    title: "동일 provider 안전 윈도우 재시도",
    where: "본 프로젝트 · 출력 전달 전",
    trigger:
      "스트림이 이미 수립됨(받음 200) 이후에 문제가 생김: 연결 중간 끊김, 공급자 overloaded, 스트림 내의 429 / 5xx 오류 이벤트——그리고 하나의 token 아직 호출자에게 전달하지 않음.",
    skips:
      "할당량 소진(402 / insufficient_quota, 폴링의 설정 전환에 맡김), 컨텍스트 과대(413 / context length, 압축에 맡김),400 / 401 / 403 / 404 / 422 결정적 거부는 모두 재시도하지 않습니다.",
    desc: "같은 설정에서 같은 요청을 재생합니다. 아직 어떤 출력도 전달하지 않았으므로, 재생은 모델 출력이나 도구 실행을 중복하지 않습니다.",
    attemptsLabel: "재시도 횟수",
    defAttempts: 2,
    defInterval: "0.5s→1s 지수(상한 4s)",
    offHint: "-1 = 스트림 끊김은 바깥 계층의 의도 재실행에 바로 맡김",
  },
  breaker: {
    title: "폴링 차단",
    where: "본 프로젝트 · 프로세스 레벨, 전역에 하나",
    trigger:
      "일시적 실패(429,5xx, 네트워크 오류)가 연속으로 임계값까지 누적되면 차단; 잔액 부족(402), 키 무효(401 / 403), 모델 없음(404)와 같은 결정적 실패는 임계값과 무관하게 처음부터 차단됩니다.",
    skips: "한 번 성공하면 0으로 초기화되므로, 가끔 말썽인 설정이 서서히 쌓여 차단되지는 않습니다.",
    desc: "차단되면 쿨다운에 진입하고, 쿨다운 기간에는 폴링이 이 설정을 건너뜁니다. 상태는 DB에 저장되어 재시작해도 유지됩니다.",
    attemptsLabel: "연속 몇 회 실패 시 차단",
    defAttempts: 3,
    defInterval: "1min→5min→30min 단계",
    offHint: "-1 = 일시적 실패는 절대 차단되지 않음(결정적 실패는 여전히 차단)",
  },
  intent: {
    title: "의도 재실행",
    where: "본 프로젝트 · 프로세스 레벨, 전역에 하나",
    trigger:
      "앞의 몇 계층이 모두 막아내지 못함:worker 로 model_error 마무리——내부 계층 재시도를 모두 소진했거나, 스트림이 출력 전달을 시작한 뒤 끊긴 경우(그때는 재생이 안전하지 않아 전체를 다시 시작할 수밖에 없음).",
    skips: "할당량 소진은 이미 폴링의 설정 전환으로 처리되므로 여기서 재실행하지 않습니다; 작업이 일시정지되면 / 종료 / 마무리에 진입하면 즉시 양보하며 백오프 시간을 차지하지 않습니다.",
    desc: "의도 전체를 처음부터 다시 실행합니다. 가장 바깥 계층이며, 한 번 재실행하면 안쪽 계층들의 횟수가 다시 곱해집니다.",
    attemptsLabel: "재실행 횟수",
    defAttempts: 2,
    defInterval: "고정 3s",
    offHint: "-1 = 재실행하지 않고 해당 의도를 바로 다음으로 판정: blocked",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** 밀리초의 사람이 읽기 쉬운 표현으로, 입력란 옆 표시에만 사용해 0을 세지 않게 합니다. */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 제어 숫자 입력: 빈 문자열 ↔ 0, 중간 상태("-","1e")를 그대로 로컬에 두고 상위를 건드리지 않습니다. */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 상위가 값 전체를 교체할 때(정책을 읽거나 설정을 전환) 따라갑니다; 직접 입력할 때는 여기로 오지 않으며,
  // 그때는 value 이미 로컬 텍스트와 같음 parse 이후의 결과.
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 한 계층 재시도의 두 노브.idPrefix 같은 페이지에 여러 번 나타날 때 유지하기 위함 label 의 htmlFor. */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true = 설정 드로어의 간략 버전: 펼침 설명을 생략하고 「어떤 오류가 이 계층으로 오는지」 한 문장만 남깁니다 */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* 어떤 오류가 이 계층으로 오는지, 상태 코드까지 구체적으로——노브를 입력해도 효과가 보이지 않으면, 대개 오류가 애초에 이 계층에 해당하지 않는 것입니다. */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">트리거</span>:{meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">이 계층을 거치지 않음</span>:{meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`기본값 ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            간격 ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="기본 백오프"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `고정 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">비우면 = 기본값 사용;{meta.offHint}.</p>}
    </div>
  );
}

/** 모델 설정 드로어의 세 계층 재정의(엔드포인트를 따르는 그 세 계층). */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">재시도 재정의</Label>
        <p className="text-muted-foreground text-xs">
          이 설정에만 적용되어 「재시도와 백오프」의 전역 기본값을 재정의합니다. 각 칸을 비우면 = 전역 따름; 횟수 입력 -1 = 이 계층 재시도 끄기;
          간격을 입력하면 고정 간격이 지수 백오프를 대체합니다. 차단과 의도 재실행은 프로세스 레벨이라 전역 페이지에서만 조정할 수 있습니다.
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「재시도와 백오프」tab: 다섯 계층의 전역 기본값. */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`재시도 정책 읽기 실패:${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // 백엔드가 범위를 벗어난 값을 구간 안으로 조정해 되돌려주며, 되돌아온 값으로 바로 갱신해 보이는 그대로 저장됩니다.
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("저장되어 즉시 적용됩니다(실행 중인 이번 호출은 여전히 이전 파라미터 사용)");
    } catch (e) {
      toast.error(`저장 실패:${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> 재시도 정책 읽는 중…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        모델 호출 한 번의 실패는 다섯 계층 재시도를 안쪽에서 바깥쪽 순서로 차례로 거칩니다:
        <span className="text-foreground"> 연결 수립 → 빈 응답 → 동일 provider 안전 윈도우 → 폴링 차단 → 의도 재실행</span>
        . 내부 계층을 소진해야 바깥 계층 차례이므로 횟수는
        <span className="text-foreground">곱해짐</span>
        의——각 계층을 최대로 올리면 한 번의 변동으로 수십 번의 요청을 소모할 수 있습니다.
        전부 비우면 현재 기본값이며, 이 페이지가 없을 때의 동작과 완전히 동일합니다. 앞 세 계층은 각 모델 설정에서 개별적으로 재정의할 수 있습니다.
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          저장
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          전체 기본값 복원
        </Button>
      </div>
    </div>
  );
}
