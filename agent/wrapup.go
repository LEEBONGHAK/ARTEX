package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 수습 프롬프트(wrap-up / settlement prompt):agent가【스텝 소진(MaxTurns)】또는
// 【타임아웃(run_seconds/MaxDuration)】으로 종료될 때, SDK의 settlement 단계가 이 프롬프트를 주입해,,
// agent가 먼저 식별했지만 쓰지 않은 내용을 저장하고、요약 한 문장을 출력하게 해, 미완성을 피한다.
//
// 각 agent의 수습 프롬프트는 백엔드에서 필요시 덮어쓸 수 있고(agents.wrapup_prompt에 저장), 비우면 여기의
// 내장 기본값을 쓴다. 【프롬프트 본문】만 편집 가능;어느 도구를 비활성화할지、수습 자체에 몇 라운드 예산을 줄지는 코드 고정 정책이다.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// 내장 기본 수습 프롬프트, agent key로 색인한다. worker는 역사적으로 하드코딩된 settleWrapUpPrompt
// (worker.go에 정의), planner/mainagent는 각자 한 버전씩;명중 안 한 것(커스텀 agent)은 범용 폴백으로 간다.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: 각 agent 수습 단계【자체】의 라운드 예산 내장 기본값(백엔드에서 >0으로 덮어쓰기 가능).
// 모두 10라운드를 줘, 수습 단계에 충분한 스텝이 있어 저장되도록 보장한다. 명중 안 하면 genericWrapupTurns로 간다.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "你本轮规划的步数即将用尽——注意只是【这一轮】结束,系统之后仍会随态势变化再次唤醒你继续规划,并非任务终止,你无需在此收束整个规划。请把本轮已经想清楚的结论落地、别让这一轮白跑,但也【不要为了收尾硬凑意图】(本轮 0 个意图仍是完全正常的结果)：(1) 若已判断出【当前就该派发】的探索方向,用一次 add_intent 批量提交(想好的别憋着不发);(2) 对已被某发现/事实证明达成的目标,调 prove_goal 标记 met(别漏判);(3) 若识别出需要分步的串行利用链,用 TodoWrite 记下,便于下次唤醒接着派。做完直接结束本轮,无需输出总结文本。"

const mainAgentWrapUpDefault = "你的步数即将用尽,本次交互就要结束。不要再发起新的探索/操作。请**单独用一句话纯文本**向用户总结当前进展、关键结论,以及建议的下一步。"

const genericWrapUpDefault = "你即将因预算耗尽被终止。请先把已完成但未落库的结果写回,再**单独用一句话纯文本**总结你做了什么、得到哪些关键结论(这句会作为本次运行的结果展示)。"

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- 작업 레벨 타임아웃 수습 문구（docs/任务级超时与收尾设计----------
//
// per-run 수습 문구와는【두 세트】다:per-run은 "이번 run의 예산이 다 떨어졌다"；작업 타임아웃은
// ""전체 작업이 마감에 도달、곧 종료"다. 의미가 종종 반대다（특히 planner:per-run은 "멈추지 말고 계획 계속"이라 하고,
// 작업 타임아웃은 "마감 도달 시 계획 중단、최종 판정"이라 한다）. worker/planner에만 설정한다.

// WrapupTaskTimeoutOverride / …TurnsOverride:작업 타임아웃 수습 문구와 라운드 수의 DB 덮어쓰기
// 에 wire, worker/planner만）.
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是你这次 run 的预算，是整场探索到点了）。这是最后机会：(1) 把你已识别但还没写回的内容【全部】落库——新资产 insert_assets、探索结论/事实 record_fact、确认漏洞 report_finding；(2) 不要再启动任何新命令/探测；(3) **最后单独用一句话纯文本**总结你在本意图上的关键结论。"

const plannerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是本轮，是整个任务终止）。请基于当前【全部】事实与发现，做最后一次目标判定：对已被证据证明达成的目标调 prove_goal 标记 met（别漏判）。**不要再生成任何新意图**（此时派意图也不会再被执行）。判定完即收束，无需输出总结文本。"

// TaskTimeoutWrapupDefault 어떤 agent의 작업 타임아웃 내장 기본 수습 문구를 반환한다（백엔드 플레이스홀더/기본값 복원 제공）.
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 미설정(mainagent/chat)이면 빈 문자열 반환
}

// resolveTaskTimeoutWrapup:DB 덮어쓰기(비어있지 않음) > 내장 기본값. 빈 문자열은 그 agent에 작업 타임아웃 문구가 없음을 의미
// （worker/planner가 아님）, 이때 호출 측은 per-run 문구로 폴백해야 한다.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 기본적으로 per-run 라운드 수를 그대로 사용
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → 이번 run이 작업 deadline에 조여짐:Timeout 수습 때문=작업 마감→작업 타임아웃 문구；
//     MaxTurns 수습 때문=조임 윈도우 내 스텝 먼저 소진、작업이 몇 분 남음→per-run 문구로 폴백.
//   - clamped=false → 작업이 아직 이름:두 reason 모두 per-run 문구를 쓴다（즉 wrapupSettlement로 퇴화）.
//
// harness의 PromptByReason에 넘겨 수습 시【실제】reason으로 현장에서 고르게 하면, build 시점의 오매칭이 없다.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // 폴백(비 clamped일 때 두 reason의 값이기도 함)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // 작업 마감
				harness.ReasonMaxTurns: perRun, // 스텝 먼저 소진、작업 시간 남음
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
