package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// 이 파일은 P2「크로스 작업 편성 도구 세트」를 구현한다(docs/跑分编排 §2 P2). 이것들은 host 도구다——필요한 것은
// Manager(임의 작업의 Store)、Engine(일시정지)에 접근、그리고 작업 생성 흐름이라, server 계층에 산다.
// 읽기류 도구는「기존 per-task 도구」를 대상 작업의 store에서 돌도록 리다이렉트한다(임시 ToolSet
// 그리고 그에 대응하는 도구를 Call하여), 완전히 동일한 로직을 재사용한다;제어류(spawn/pause)는 Manager/Engine을 직접 호출한다.
// 그것들은 트래픽 도구처럼 tools 표에 seed되고、agent별로 바인딩된다(편성 agent에만 바인딩해야 보인다).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // 플랫폼 작업 도구(skill/도구/MCP 생성·수정, Auto용)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 로드 실패:: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id 为必填"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task 不存在: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 범용 깨우기（전용 콜백 없는 쓰기 작업이 이것을 거침；읽기 도구는 no-op）
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → 한 건「사람이 전략 힌트 N건 추가함:…」을 기록해 트리거하고 planner를 깨움 planner
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"列出所有任务(id/描述/目标/状态/运行时长/父任务/LLM 配置)，编排 agent 用它掌握全局、看哪些任务卡太久、各自用哪个 LLM。运行时长：运行中=创建→现在，终态=创建→最后活动(秒)。llm_profile：任务 planner/worker 用的配置名，(激活配置)=跟随全局激活。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(激活配置)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(已删除)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"列出可用的 LLM 配置(profile)：id、名称、模型、格式、是否为当前激活配置。用 id 给 spawn_task 的 llm_profile_id 参数指定子任务专属 LLM（如侦察用便宜模型、利用用强模型）。不含 API Key。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"新建一个子任务并启动探索引擎，返回 task_id。用于把一件事(如一道题/一个目标)派成独立任务。parent_ref 可选：填当前编排关联的父任务 id 做父子关联。",
		objSchema(map[string]any{
			"description":            strParam("任务描述(简短标题)"),
			"goal":                   strParam("任务目标(要达成什么)"),
			"parent_ref":             strParam("可选：父任务 id(做父子关联)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("可选：只读继承的来源任务 id 列表(最多 %d 个)。子任务可只读引用这些任务已探明的资产/结论作为起点；与 parent_ref 的纯父子指针不同，这是内容继承。", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "可选：指定本子任务 planner/worker 用的 LLM 配置 id(见 list_llm_profiles)；留空则继承父任务、再回退全局激活配置"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "可选：任务级超时(秒)。到点后触发优雅收尾并进入 timeout 终态；留空或 0 = 不限时"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "可选：planner 心跳触发间隔(秒)。距上轮规划结束/任务开始满该值且期间无触发 → 触发一轮规划(兜底死锁 + 唤醒去监督飞行中的 worker)。留空或 0 = 默认 600(10min)；"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "可选：对于简单任务可开启，创建时直接下发一条种子意图(内容=描述+目标)让 worker 免等首轮 planner 直接开跑测试；默认 false(走标准先规划再执行)。"},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "이름 없는 작업"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal 为必填"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 읽기 전용 상속 소스 작업:수량 상한 + 각 id 유효/중복 제거/존재, 검증 규칙은 HTTP 작업 생성과 동일.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("关联任务最多选择 %d 个", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("关联任务 id 无效或重复"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("关联任务 #%d 不存在", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 配置 #%d 不存在或未设置 API Key", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// 공유되는 생성 후 흐름으로, HTTP 작업 생성(server.go createTask)과 같은 구간을 재사용: launchTask:
			// seed + seed + 백그라운드에서 보이게 목표 분해(제0라운드/LLM 스텝/goal 하나씩) + engine.Run.
			// seed_first_intent 기본 false(표준은 먼저 계획 후 실행);간단한 작업은 켜서 바로 work 하나를 내려보내 테스트할 수 있다.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "暂停指定任务(停止其 planner/worker 循环)。",
		objSchema(map[string]any{"task_id": strParam("要暂停的任务 id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task 不存在: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "读指定任务的探索图总览(同 graph_overview：资产计数/frontier/发现/覆盖等)，用 task_id 指定任务。",
		objSchema(map[string]any{"task_id": strParam("任务 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "读指定任务的确认漏洞(含 flag/PoC；每条带 id/task_id/intent_id/vulnclass/severity/摘要/状态)，用 task_id 指定任务。",
		objSchema(map[string]any{"task_id": strParam("任务 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "给指定任务注入战略提示(该任务的 planner 下轮生成意图时会读到)。\n"+
		"★优先批量：多条提示放进 hints 数组一次提交（返回 ids 数组，与 hints 等长同序，失败项 id=0）；单条则省略 hints 直接给顶层 text。",
		objSchema(map[string]any{
			"task_id":      strParam("任务 id"),
			"hints":        map[string]any{"type": "array", "description": "【优先用这个】提示数组，每个元素字段同顶层（text/asset_ids/traffic_refs）。", "items": objSchema(map[string]any{"text": strParam("提示内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[单条] 提示内容"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "锚定的资产 id（可选，0/1/多个；该任务内的资产 id）"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"看指定任务里某个 work(意图)的执行过程：get_task_worker_trace(task_id, intent_id) 看步骤摘要；再带 step_ids=[...] 取那几步完整内容(一次最多 5 个,多传只返回前 5 个)。",
		objSchema(map[string]any{
			"task_id":   strParam("任务 id"),
			"intent_id": map[string]any{"type": "integer", "description": "意图 id(该任务里的 work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "可选：要取完整内容的步骤 id(一次最多 5 个,多传只返回前 5 个,其余在 omitted_step_ids 里列出)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "列出指定任务里跑过哪些 work(意图) + 各自步数，用于发现哪些 work 值得翻看(再用 get_task_worker_trace)。",
		objSchema(map[string]any{"task_id": strParam("任务 id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "在指定任务里按关键字搜索所有 work 的执行过程(返回命中步骤摘要 + intent_id)。",
		objSchema(map[string]any{"task_id": strParam("任务 id"), "q": strParam("搜索关键字")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"读指定任务里某个探索图节点的完整内容(发现/事实/意图/目标：摘要 + 详情/证据/PoC)。id 为探索节点 id(如 report_finding 返回、或 list_task_findings 里的 id)。写漏洞报告前用它取该漏洞的完整证据。",
		objSchema(map[string]any{
			"task_id": strParam("任务 id"),
			"id":      map[string]any{"type": "integer", "description": "探索图节点 id(非资产 id)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"为已登记的漏洞写入/更新【详细报告】(Markdown 全文,整段覆盖旧内容)。finding_id 传 report_finding 返回的那个 id(\"finding recorded: <id>\" 里的数字)。报告建议包含:漏洞概述、影响与危害、复现步骤、证据/PoC、修复建议。",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "目标漏洞 id(report_finding 返回的 id)"},
			"report":           strParam("详细报告全文,Markdown 格式"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic 返回的证据 version；用于防止报告覆盖新的证据变更"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 「숫자 또는 숫자 문자열」파싱을 재사용
			if nodeID <= 0 {
				return actool.Errorf("finding_id 无效"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("未找到 finding_id=%d 对应的漏洞记录(先用 report_finding 登记)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (그것은 본래
	// 플랫폼을 조작하게 돼 있다). SeedTool은 첫 삽입에 적용;구버전 DB에 이미 seed된 행은 seedAutoDefaultBindings가 보충 바인딩한다.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces worker에서 기본 해제(일회성)
	s.seedWorkerReadbackRebind()  // 구버전 마이그레이션 오삭제 수정:search_all_worker_traces/get_worker_trace/node_detail를 worker에 다시 보충 바인딩(일회성)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals 프롬프트에「작업 제약 추출」스텝 추가 → 구버전 DB에 새 기본값 한 버전 추가(일회성)
	s.reseedMainAgentPrompt()         // mainagent 프롬프트에「목표 달성 후 add_intent로 목표 생성 여부 반문」추가(일회성)
	s.reseedPlannerPrompt()           // planner 프롬프트:「0 의도」정당 사유 재작성 + 정량 검수 체크 추가(일회성)
	s.reseedWorkerPrompt()            // worker 프롬프트:부정 결론 증거 문턱 추가(일회성)
	s.seedReporterAgent()             // 「보고서 작성」agent + 도구 바인딩 + finding 트리거 사전 배치(일회성)
	s.upgradeReporterTriggerMessage() // 구버전 DB 보충 마이그레이션:reporter가 evidence_version을 회신하도록(일회성)
	s.seedFindingTrafficTools()       // 선택적 증거 파라미터 및 읽기 전용 증거 도구 추가, 사용자 설정 보존
	s.seedFindingWorkflowTools()
	// 주:pentest의 기본 도구 바인딩은 마이그레이션 불필요——BuiltinToolSeeds가 완전 새 초기화 시
	// list_assets/insert_assets/report_finding/list_findings/list_companies 도
	// pentest pentest와 함께 seed했다（프로젝트에 아직 구버전 DB 없어, 마이그레이션 안 함）.
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task 의 llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 동시에 일부 내장 agent 도구를 코드 기본값으로 갱신한다:
	//   - goal_met:구버전 DB가 seed한 설명에 "이번 라운드 계획 종료"라는 오해 소지가 있어, planner가 그것을
	//     빈 라운드 종료"의 수단으로 삼아、막 시작하자마자 작업 전체 완료로 오판한다.
	//   - insert_assets:related 입력 파라미터 추가(자산이 현재 작업과 관련 있는지 표시、커버리지에 넣을지 결정),
	//     SeedTool SeedTool은 첫 삽입only라, 구버전 DB에 이미 seed된 schema는 아니면 이 새 파라미터를 받지 못한다.
	//   - list_facts:페이지네이션으로 변경, limit/before/q 입력 파라미터 추가；구버전 DB에 이미 seed된 빈 schema는 아니면
	//     도구 관리 페이지에「파라미터 없음」으로 표시되고, 모델도 이 파라미터 설명들을 받지 못한다.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform 도구 schema를 코드 기본값으로 갱신 완료(일회성)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met planner 바인딩 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt goals 목표 분해기의 프롬프트를【현재 코드 기본값】으로 갱신한다——기본 본문에 새로 추가된 것은
// 「먼저 작업 제약 추출(set_constraints) 후 목표 분해」이 스텝이고, SeedPromptIfEmpty는 첫 삽입only라, 구버전 DB
// 에 이미 있는 version 1은 이 스텝을 받지 못한다. 여기서 버전 관리로【새 버전 하나 추가】하고 전환한다(ResetPromptToDefault),(ResetPromptToDefault),
// 구 버전은 여전히 히스토리에 남아, 사용자가 커스터마이즈했다면 버전 기록에서 복구 가능하다. settings flag 가드 → 한 번만 실행;;
// 이후 기본값이 또 바뀌면 이 flag를 bump한다. 완전 새 DB는 처리 불필요(SeedPromptIfEmpty가 이미 최신 기본값을 seed함).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 완전 새 DB가 아직 agent 행을 만들지 않았을 때는, seedPrompts가 바로 최신 기본값을 seed하므로, 이 마이그레이션 불필요
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 완전 새 DB는 seedPrompts가 이미 최신 기본값을 seed함 → 현재 버전이 이미 코드 기본값과 같으니, 중복 버전을 다시 추가할 필요 없음.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals 프롬프트를 새 기본값으로 재갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] goals 프롬프트에 새 기본값 버전 추가 완료(작업 제약 추출 스텝 추가, 일회성)")
}

// reseedMainAgentPrompt mainagent 프롬프트를【현재 코드 기본값】으로 갱신한다——기본 본문에 새로 추가된「목표 전부
// 달성 후 add_intent로 의도를 직접 투입할 때, 정식 목표로 등록할지 사람에게 반문」이라는 안내이고, SeedPromptIfEmpty는 첫 삽입
// only,only라, 구버전 DB의 기존 버전은 받지 못한다. 버전 관리로【새 버전 하나 추가】하고 전환한다(ResetPromptToDefault), 구 버전은 여전히
// 히스토리에 남아, 사용자가 커스터마이즈했다면 버전 기록에서 복구 가능하다. settings flag 가드 → 한 번만 실행. 완전 새 DB는 처리 불필요
// (SeedPromptIfEmpty (SeedPromptIfEmpty가 이미 최신 기본값을 seed함). reseedGoalsPrompt와 완전히 동형.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 완전 새 DB가 아직 agent 행을 만들지 않았을 때는, seedPrompts가 바로 최신 기본값을 seed하므로, 이 마이그레이션 불필요
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 완전 새 DB는 seedPrompts가 이미 최신 기본값을 seed함 → 현재 버전이 이미 코드 기본값과 같으니, 중복 버전을 다시 추가할 필요 없음.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent 프롬프트를 새 기본값으로 재갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] mainagent 프롬프트에 새 기본값 버전 추가 완료(목표 달성 후 목표 생성 반문 추가, 일회성)")
}

// reseedPlannerPrompt planner 프롬프트를【현재 코드 기본값】으로 갱신한다——기본 본문을 간소화 리팩터링했고,「절제」를 강등해
// 단순 중복 제거만、「깊이가 커버리지보다 우선」「하드 바텀라인:목표 미달성이고 실행 중 의도가 없으면 반드시 산출」추가、부정 결론 복검에 상한 추가.
// 기본값에 실질 변경이 있을 때마다 아래 flag를 bump한다(현재 v2) 존량 구버전 DB가 다시 한 번 갱신되게. SeedPromptIfEmpty는 첫 삽입only라, 구버전 DB의 기존 버전은 받지 못하므로, 버전 관리를 쓴다
// 【새 버전 하나 추가】하고 전환한다(ResetPromptToDefault), 구 버전은 여전히 히스토리에 남아, 사용자가 커스터마이즈했다면 버전 기록에서
// 복구 가능하다. settings flag 가드 → 한 번만 실행. 완전 새 DB는 처리 불필요(SeedPromptIfEmpty가 이미 최신 기본값을 seed함). 그리고
// reseedGoalsPrompt reseedGoalsPrompt와 완전히 동형.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 완전 새 DB가 아직 agent 행을 만들지 않았을 때는, seedPrompts가 바로 최신 기본값을 seed하므로, 이 마이그레이션 불필요
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 완전 새 DB는 seedPrompts가 이미 최신 기본값을 seed함 → 현재 버전이 이미 코드 기본값과 같으니, 중복 버전을 다시 추가할 필요 없음.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner 프롬프트를 새 기본값으로 재갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] planner 프롬프트에 새 기본값 버전 추가 완료(간소화 리팩터링+절제 강등 중복제거+깊이 우선+부정 복검 상한, 일회성)")
}

// reseedWorkerPrompt worker 프롬프트를【현재 코드 기본값】으로 갱신한다——기본 본문 record_fact 구간에서「부정류 결론
// 관찰+탐색적 읽기 작성」문장 전체를 삭제、그리고 confidence(observed/inferred)와「이 의도의 수단을 소진했는지」를 분리(이것들은 Planner를 오도하기 쉬움),),
// 동시에 facts 배열 분할을「서로 완전히 독립、병합 불가」한 극소수 예외로 조인다. bump flag를 v3로 올려 존량 구버전 DB가 다시 한 번 갱신되게 한다.
// SeedPromptIfEmpty SeedPromptIfEmpty는 첫 삽입only라, 구버전 DB의 기존 버전은 받지 못하므로, 버전 관리로【새 버전 하나 추가】하고 전환하며, 구 버전은 여전히 히스토리에 남아 복구 가능하다.
// settings flag 가드 → 한 번만 실행. 완전 새 DB는 처리 불필요. reseedGoalsPrompt와 완전히 동형.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 완전 새 DB가 아직 agent 행을 만들지 않았을 때는, seedPrompts가 바로 최신 기본값을 seed하므로, 이 마이그레이션 불필요
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 완전 새 DB는 seedPrompts가 이미 최신 기본값을 seed함 → 현재 버전이 이미 코드 기본값과 같으니, 중복 버전을 다시 추가할 필요 없음.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker 프롬프트를 새 기본값으로 재갱신 실패: %v", err)
		return
	}
	log.Printf("[prompts] worker 프롬프트에 새 기본값 버전 추가 완료(컨텍스트 조회 구간을 list_assets/list_findings로 수렴, list_facts/node_detail/asset_neighbors 제거, 일회성)")
}

// reporterToolCallMessage 반드시 무조건 먼저 get_finding_traffic을 한 번 읽은 뒤 보고서를 쓰도록 요구해야 한다.
// 이 도구는 읽기 전용이고、「캡처 스위치에 의존하지 않음」, 자동 바인딩을 켜든 끄든 수동 바인딩된 증거를 읽을 수 있다. 여기서
// 「자동 바인딩을 켜야 읽는다」로 쓰면, 기본 꺼짐 설정에서 reporter가 evidence_version을 전달하지 않고, evidence_version,
// SetFindingReportVersionByNodeID SetFindingReportVersionByNodeID가 legacy 의미로 -1을 써, 취약점 상세와 Markdown 내보내기가
// 그때부터「증거가 변경됨, 보고서 업데이트 대기」에 상주하고, UI에는 그것을 지울 입구가 하나도 없다.
const reporterToolCallMessage = "上面刚有一个漏洞被 report_finding 登记。请读取返回 JSON 的 finding_id（独立漏洞记录 ID）与 finding_node_id（探索节点 ID），" +
	"先用 get_finding_traffic(finding_id) 读取当前证据清单及其 version（空清单是正常情况，照常写报告）；" +
	"若运行指引启用自动绑定，在读取前先核实并关联本次漏洞的流量。节点详情使用 finding_node_id。" +
	"最后调用 update_finding_report(finding_id=finding_node_id, report, evidence_version=实际读取版本) 保存，" +
	"evidence_version 必须传，否则报告会被永久标记为待更新。不要混用两种编号。"

// 구버전 트리거 메시지(0.3.8 및 이전). 여전히 그것과 글자 그대로 같은 기록만 마이그레이션에 덮어쓰이고, 사용자가 수정한 것은 원본 유지.
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage 구버전 DB에서 여전히 기본 문구인 reporter 트리거 메시지를 새 버전으로 갱신한다.
// seedReporterAgent reporter_agent_seed_v1 가드를 받고 새 agent를 만들 때만 트리거를 쓰므로,
// 업그레이드된 DB는 새 문구를 받지 못한다 —— 도구 schema는 seedFindingTrafficTools가
// evidence_version, evidence_version을 보충했지만, reporter에게 그것을 쓰라고 알려주는 게 아무것도 없다. 일회성이고, 수정되지 않은 문구만 덮어쓴다.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 한 번만 시도
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] 트리거 읽기 실패: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // 사용자가 수정했거나 finding 트리거가 아니면, 건드리지 않음.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] 트리거 메시지 업그레이드 실패: %v", err)
			return
		}
		log.Printf("[reporter] 트리거 메시지를 읽고 회신하도록 업그레이드함: evidence_version")
	}
}

// seedReporterAgent 「보고서 작성」커스텀 agent를 사전 배치한다(builtin=false, UI에서 편집/삭제 가능):
// update_finding_report + 작업 조회 도구를 바인딩하고,「report_finding이 호출되면 트리거」라는
// 트리거를 단다 —— 취약점 하나를 등록할 때마다 그것을 깨워 상세 보고서를 쓰게 한다. 일회성(settings flag 가드):사용자가 삭제한 뒤에는 다시 만들지 않는다.
// 의존:orchestration 도구가 이 함수 위쪽에서 SeedTool로 입고됐으므로, 바인딩 가능하다.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 성공 여부와 무관하게 한 번만 시도

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key key가 이미 점유됨(사용자가 수동 생성)——덮어쓰지 않음
	}
	a, err := s.m.pg.CreateAgent("reporter", "보고서 작성",
		"취약점 상세 보고서 작성:취약점 발견 시 자동 트리거, 증거와 실행 과정을 조회한 뒤 Markdown 보고서를 쓰고 회신한다.")
	if err != nil {
		log.Printf("[reporter] agent 생성 실패: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt 실패: %v", err)
	}
	// 트리거 실행 정책:parallel + none —— 취약점 하나에 보고서 하나、여러 finding은 동시에 각자 쓴다.
	// merge merge는 반드시 none이어야 한다:아니면(기본 all) 한 무더기 finding이 한 번의 실행으로 병합돼, 병렬이 의미 없어진다.
	// maxParallel=5:동시에 최대 5개 보고서 세션, 순간적으로 너무 많은 LLM 호출을 피한다.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] 트리거 실행 정책 설정 실패: %v", err)
	}
	// 그것이 필요한 도구를 바인딩:보고서 쓰기 + 증거/실행 과정/상황 읽기.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] 도구 바인딩 실패: %v", err)
	}
	// 트리거:report_finding이 호출되면 트리거（도구가 "finding recorded: <id>"를 반환하며 finding_id를 실음,
	// 작업 id도 트리거 메시지에 있음）.
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] 트리거 생성 실패: %v", err)
	}
	log.Printf("[reporter] 「보고서 작성」agent + finding 트리거 사전 배치 완료")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker→planner 기본 바인딩 전환
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope 기본 바인딩 실패: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope 바인딩 해제 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s worker에서 바인딩 해제 실패: %v", k, err)
			return // 에러 시 flag를 남기지 않음, 다음 시작에 재시도
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: 추가: node_detail
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 회람/상세 도구 보충 바인딩 실패: %v", err)
		return // 에러 시 flag를 남기지 않음, 다음 시작에 재시도
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 구 자산 도구명 교체, 추가: insert_assets/add_company_scope
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// 자산 도구:Auto가 플랫폼을 조작할 때 자주 자산을 보고/등록、회사 범위를 관리한다.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] 기본 바인딩 실패: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
