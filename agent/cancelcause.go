package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "사용자가 작업을 일시정지함",
		"사용자가 작업 제어 인터페이스(POST /api/tasks/{id}/control, action=pause)로 작업을 일시정지함. 이번 Planner/Worker 실행은 능동 취소됨; 실행 중인 의도는 frontier(open)로 되돌아가며, 작업 재개 후 다시 수령해 처음부터 실행")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "오케스트레이션 Agent가 작업을 일시정지함",
		"오케스트레이션 Agent가 pause_task 도구를 호출해 이 작업을 일시정지함. 이번 Planner/Worker 실행은 능동 취소됨; 실행 중인 의도는 frontier(open)로 되돌아가며, 재개 후 다시 실행")
	AbortTaskDeleted = cause("task_deleted", "작업이 삭제됨",
		"작업을 삭제 중(DELETE /api/tasks/{id}), 삭제 배리어가 이 작업의 실행 중인 Planner、Worker、메인 Agent를 취소함; 이번 실행 결과는 더 이상 사용되지 않음")
	AbortPausedOnReload = cause("paused_on_reload", "백엔드가 작업의 일시정지 상태를 복원함",
		"백엔드 시작 시 데이터베이스에 영속화된 상태에 따라 작업 일시정지를 복원함. 이번 실행은 취소됨; 정상적인 경우 복원 단계에는 실행 중인 Agent가 없음")
	AbortGoalMet = cause("goal_met", "플래너가 작업 목표 달성으로 판정함",
		"플래너가 작업 목표 달성으로 판정하고 작업을 done으로 설정한 뒤, 여전히 실행 중인 Worker를 취소함; 이 의도들은 실패가 아니라 stopped로 표시됨")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "작업 타임아웃 수습 대기 시간이 소진됨",
		"작업이 timeout에 도달한 뒤 실행 중인 Worker의 우아한 수습을 기다렸으나 90초 drain 유예로도 부족해 하드 취소를 실행함; 의도는 exhausted로 표시되고, 수습 단계에서 이미 기록된 사실과 자산은 보존됨")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "플래너가 이 의도를 종료함",
		"플래너가 kill_work를 호출해 이 의도를 능동 종료함, 보통 방향이 어긋났거나 더 진행할 가치가 없음을 의미; 의도는 stopped로 표시되며 자동으로 다시 수령되지 않음")
	AbortWorkPausedByUser = cause("work_paused_by_user", "사용자가 이 Worker 의도를 일시정지함",
		"사용자가 실행 중인 Worker를 일시정지함. 이번 호출은 취소되고 의도는 paused로 전환됨; 이미 등록된 의도、사실、취약점、활동 기록은 전부 보존되며, 재개 후 처음부터 다시 실행")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "사용자가 이 Worker 의도를 삭제함",
		"사용자가 실행 중인 Worker를 삭제함. 이번 호출은 취소됨; Worker가 쓰기 구역을 빠져나온 뒤 서버가 사용자가 선택한 삭제 모드로 이 의도를 처리함——가짜 삭제는 삭제됨으로만 표시하고 모든 산출을 보존, 하드 삭제는 이 의도와 그것만이 지탱하는 하위 노드를 캐스케이드 제거함")
	AbortWorkFinished = cause("work_finished", "Worker가 정상 종료되고 context를 해제함",
		"Worker가 정상 종료되어 엔진이 detachWork에서 그 context 자원을 해제함. 이것은 실행 중단이 아님; 중단 메시지에 나타난다면 취소와 종료 이벤트 간 경합이 발생한 것")
	AbortPausedRaceGuard = cause("paused_race_guard", "작업 일시정지 중 새 실행 시작을 거부함",
		"작업이 일시정지 상태일 때 엔진이 새 실행 context 발급을 거부함, claim과 일시정지 사이의 경합으로 Worker가 계속 시작되는 것을 방지하기 위함; 이미 수령된 의도는 frontier로 되돌아감")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "사용자가 이번 대화 턴을 중지함",
		"사용자가 중지를 클릭해 이번 메인 Agent 또는 세션 Agent 실행을 능동 중단함. 이미 생성된 활동 기록은 보존되며, 다음 메시지를 계속 보낼 수 있음")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "작업 일시정지로 메인 Agent 대화를 중단함",
		"사용자가 작업을 일시정지할 때 실행 중인 메인 Agent 대화도 함께 취소됨. 이미 생성된 활동 기록은 보존됨; 작업 재개 후 이번 턴 메시지를 자동 재생하지 않음")
	AbortChatTurnFinished = cause("chat_turn_finished", "이번 대화 턴이 정상 종료되고 context를 해제함",
		"이번 대화 턴이 정상 종료되어 서버가 해당 턴의 context 자원을 해제 중. 이것은 실행 중단이 아님; 중단 메시지에 나타난다면 취소와 종료 이벤트 간 경합이 발생한 것")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "백엔드 프로세스가 종료 중",
		"백엔드 프로세스가 SIGINT 또는 SIGTERM을 받아 재시작、업데이트 또는 종료 중. 실행 중인 모든 Agent가 취소됨; 재시작 후 잔존한 running 의도는 open으로 리셋되어 다시 실행됨")
	AbortRunHardTimeout = cause("run_hard_timeout", "단일 실행의 하드 타임아웃 백스톱이 발동됨",
		"단일 실행이 소프트 벽시계 예산 및 추가 유예를 초과함, 모델 요청이나 어떤 도구가 오랫동안 반환하지 않아 정상적인 라운드 경계 수습을 수행하지 못했음을 의미. 중단 전 마지막으로 반환되지 않은 도구 호출을 중점 점검하세요")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "상위 context가 deadline에 도달함",
			"상위 context가 deadline에 도달했으나 설정 측이 WithTimeoutCause로 명명된 원인을 붙이지 않음: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "취소 측이 명명된 원인을 붙이지 않음",
			"상위 context가 취소됐으나 취소 측이 context.WithCancelCause로 명명된 원인을 붙이지 않음; agent/cancelcause.go에 원인을 등록하고 해당 취소 지점에 연결하세요", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
