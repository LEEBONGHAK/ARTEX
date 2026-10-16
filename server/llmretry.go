package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 재시도 정책의 서버 측 파싱, docs/LLM重试设计.md 참조. 다섯 계층:
//   - 연결 수립 / 빈 응답 / 동일 provider 안전 윈도우는「엔드포인트를 따라가는」것이고, 각 LLM 설정이 전역 기본값을 덮어쓸 수 있다
//     （profile의 어떤 항목을 비우면 전역을 상속하고, 전역도 설정 안 했으면 내장 기본값 사용）；
//   - 차단 / 의도 재실행은 프로세스 레벨이라, 전역에 하나뿐이다.
//
// 전역 정책은 DB의 settings 한 행을 한 번 읽고, 호출 지점은 모두 저빈도 경로에 있다（provider 구축、work 수습、
// 설정 저장）, 캐시를 한 겹 더 추가할 가치가 없다；차단 파라미터는 예외다——실패 경로에서 매번 읽어야 하므로
// applyRetryPolicy가 Registry에 푸시해 저장한다.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 횟수는 여기서「0=기본 / 음수=끄기」의 원래 의미를 유지한다:SDK의 MaxRetries /
		// EmptyResponseRetries EmptyResponseRetries와 완전히 동형이라, 그것이 스스로 파싱하게 하면 된다.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// 차단(폴링 쿨다운)의 기본값, llmpool 내장과 일치 —— 여기서는「사용자가 값을 설정한」경우에만 덮어쓴다.
// 의도 재실행의 기본값은 engine.go의 modelErrorRetries / modelErrorRetryBackoff 참조.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob —— 「빈 응답
// 재시도 횟수」:둘은 같은 일의 두 수단이다. SDK 그 계층은「내용 블록이 하나도 없음」을 관리하고, 수단은
// 같은 요청을 원본 그대로 재전송한다;여기서는「사고만 있고、본문도 도구도 없음」을 관리하고, 수단은 지시 하나를 덧붙여
// 모델이 기존 사고를 가지고 계속하게 한다(원본 그대로 재전송은 컨텍스트 형태로 결정되는 이런 공회전에 의미 없음). 공 판정 기준
// 이 다른 것은 SDK가「이벤트를 yield한 적 있는지」를 기준으로 하기 때문이고, 사고 증분 자체가 이벤트라서——하지만 사용자가
// 「빈 응답 재시도 몇 번」을 설정할 때 표현하려는 것은「모델이 실질 내용을 산출 안 하면 한 번 더」이고, 두 계층이 하나의 횟수를 공유해야
// 이 심상에 맞는다.
//
// 어떤 profile의 덮어쓰기가 아니라 전역 정책을 읽는다:한 run이 도중에 failover로 profile을 바꿀 수 있는데, 이것은
// 의도 전체의 총량 게이트라, 엔드포인트를 바꿀 때마다 변하면 안 된다. 의미는 SDK의 emptyRetries()와 동형이다:
// 0 = 기본값 defaultEmptyTurnNudges;-1(음수) = 공회전 이어가기 끄기;>0 = 이 값 사용.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
