package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side resolution of the retry policy. See the LLM retry design notes. Of the five layers:
//   - Connect, empty response, and the same-provider safety window follow the endpoint. Each LLM
//     configuration can override the global default (a blank profile field inherits the global value,
//     and a blank global value uses the built-in default).
//   - Circuit breaking and intent replay are process-wide and exist only as one global copy.
//
// The global policy is one settings row. Call sites are on cold paths (building a provider, finishing
// work, saving a configuration), so another cache is not worth it. Circuit-breaker parameters are the
// exception: the failure path reads them every time, so applyRetryPolicy pushes them into the Registry.

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
		// Attempt counts keep the raw meaning "0 = default / negative = off" here. The SDK's
		// MaxRetries and EmptyResponseRetries use the same shape, so they parse it themselves.
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

// Defaults for the circuit breaker (rotation cooldown), matching the values built into llmpool.
// They are overridden only when the user set a value. Intent-replay defaults are
// modelErrorRetries / modelErrorRetryBackoff in engine.go.

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
// inject (see steerHooks.Stop). It deliberately reuses layer 2's knob, the empty-response
// retry count: the two are different means for the same thing. The SDK layer covers "no
// content block at all" by resending the same request. This layer covers "thinking only,
// with neither text nor a tool" by appending an instruction so the model continues from
// the thinking it already produced (resending the same request does nothing for a stall
// caused by the shape of the context). The emptiness checks differ because the SDK asks
// whether any event was yielded, and a thinking delta is itself an event. When a user sets
// "retry an empty response this many times", they mean "try again if the model produced
// nothing substantive", and sharing one count matches that.
//
// Read the global policy rather than a profile override: a run can switch profiles mid-way
// on failover, and this cap is for the whole intent, so it should not change with the
// endpoint. The meaning matches the SDK's emptyRetries():
// 0 = the defaultEmptyTurnNudges default; -1 (negative) = disable empty-turn continuation; >0 = use that value.
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
