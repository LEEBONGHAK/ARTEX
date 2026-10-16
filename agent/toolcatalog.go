package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// 이 파일은「내장 도구」를 순수 코드에서 열거 가능、DB로 덮어쓸 수 있는 목록으로 바꾼다:
//   - BuiltinToolSeeds():세 실행 agent의 내장 도구 세트를 seed 레코드로 펼친다（key +
//     설명 + 파라미터 schema + 기본 바인딩된 agent）, 서버가 부팅 시 멱등 파종하도록 tools 표에 제공한다.
//   - ToolResolve 훅:런타임에 DB의 tools 행에 따라 이미 장착된 도구에「agent별 필터 + +
//     설명/schema 덮어쓰기 + 파라미터 기본값 주입」을 한다. key/handler는 여전히 코드 계층에 있고, DB는「산문과 기본값」만 바꾼다.
// handler（Call 동작）는 영원히 코드에서 온다——DB는 그것을 바꿀 수 없고, 모델이 보는 설명과 기본 입력 파라미터만 바꿀 수 있다.

// ToolSeed 내장 도구 하나의 파종 가능한 스냅샷이다:key는 곧 CoreTool.Name()이다（handler와 강하게 묶임,
// UI UI 읽기 전용）, Desc/Schema는 코드 속 도구 정의에서 취하고, Agents는 코드 기본이 그것을 어느 agent에 줬는지다.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), 기본 키, 변경 불가
	Desc   string         // 최상위 설명（UI에서 덮어쓰기 가능）
	Schema map[string]any // 파라미터 JSON-Schema（구조 읽기 전용, description/default는 UI에서 수정 가능）
	Agents []string       // 기본 바인딩된 agent key（worker/planner/mainagent）
}

// builtinToolsByAgent 「읽기 전용 빈 껍데기」ToolSet（nil stores）으로 각 실행 agent의
// 도메인 도구 세트를 구성한다. 도구 생성 함수는 클로저를 Spec에 넣기만 하고、생성 시점에 store를 역참조하지 않으므로, nil 안전하다——
// 이 도구들은 여기서 Name()/Description()/InputSchema()를 읽는 데만 쓰이고, 절대 Call하지 않는다.
//
// 의도적으로 SDK 범용 도구 actool.DefaultTools()를【포함하지 않는다】（Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash）:그것들은 각 agent가 고정적으로 갖고、「누구에게 바인딩」할지 취사가 없으며, 설명은 대부분 다음에 있다: Prompt()
// 안에（이 표는 Description()만 덮어, 반쪽 덮어쓰기 오도를 초래）. seed 안 함 → DB 행 없음 → ToolResolve
// 원본 그대로 통과、덮어쓰지 않아, 동작이 이전과 일치한다. artex 자신의 도메인 도구만 표에 들어가 관리 가능하다.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals（목표 분해기）는 기본으로 set_goals + set_constraints를 바인딩:그것들로 분해한 목표、
		// 추출한 작업 제약을 DB에 쓴다. mainagent와 동일한 관리형 도구를 공용하고, web에서 설명/schema 수정、agent별 체크 가능.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto 기본으로 취약점 보고 + 자산 관리 도구를 바인딩, 다른 도메인 도구는 UI에서 필요시 체크 가능.
		// 새 DB는 이 seed로 쓰이고；구 DB는 seedAutoDefaultBindings가 마이그레이션한다.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest（독립 침투 agent）는 기본 바인딩:자산 조회 / 자산 삽입 / 취약점 보고 / 취약점 조회 / 기업 조회.
		// 새 DB는 이 seed로 쓰이고；구 DB는 seedPentestDefaultBindings가 마이그레이션한다.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound:이 system 도구들은 평소대로 목록에 들어간다（web에서 보이고、수동으로 agent별 체크 가능）, 하지만
// 기본【어떤 agent에도 바인딩 안 함】——ToolResolve는 빈 바인딩 도구를 모든 agent에 대해 일률적으로 버리니, 명시적 opt-in 필요.
// 여전히 어떤 agent의 base 도구 세트에 남겨두는 이유는(예: goal_met이 PlannerTools에):하나는 seed가
// 그것을 구성해 desc/schema를 얻게 하기 위함이고, 둘은 사용자가 수동으로 다시 바인딩한 뒤 런타임 base에 그것이 있어야、ToolResolve가 유지할 수 있어서다.
//
// goal_met:prove_goal을 하나씩 거치지 않고、전역에서 직접【전체 작업 완료】를 선언, 가중치가 크고 오판 위험이 있으며, 또
// 「prove_goal이 마지막 목표를 표시 → 자동 마무리」와 중복되므로, 기본으로 어떤 agent에도 주지 않고, 필요할 때 수동으로 바인딩한다.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds 각 agent의 내장 도구 세트를 중복 제거·병합해 seed 목록으로 만든다:동명 도구（예: list_assets
// 여러 agent가 모두 가짐）은 한 건으로 합치고, Agents는 합집합을 취한다；defaultUnbound의 도구는 바인딩을 비움으로 강제한다.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 목록 편입、수동 바인딩 가능하지만, 기본으로 어떤 agent에도 안 줌（[] 저장, null 아님, 다른 도구와 일치）
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 기본 입력 파라미터 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only기본값 are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
