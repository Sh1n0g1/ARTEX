package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side resolution of the retry policy; see docs/LLM-retry-design.md. Of the five
// layers:
//   - connect / empty-response / same-provider safety window follow the endpoint:
//     each LLM configuration can override the global default (a profile field left
//     empty inherits the global value, and if the global value isn't set either, the
//     built-in default is used);
//   - circuit-breaker / intent replay are process-wide, with a single global copy.
//
// The global policy reads one settings row from the DB once; its call sites are all
// on low-frequency paths (building a provider, wrapping up work, saving config), so
// adding another cache layer isn't worth it. The circuit-breaker parameters are the
// exception — they must be read on every failure path, so applyRetryPolicy pushes
// them into the Registry for it to hold.

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
		// The counts here keep the raw semantics of "0 = default / negative = off":
		// the SDK's MaxRetries / EmptyResponseRetries are isomorphic to this, so
		// leave the parsing to it.
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

// The circuit-breaker (polling cooldown) defaults match llmpool's built-in values —
// this only overrides them when the user has actually configured a value. See
// engine.go's modelErrorRetries / modelErrorRetryBackoff for the intent-replay
// defaults.

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
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob — the
// "empty-response retry count": the two are two ways of handling the same thing.
// The SDK layer handles "not a single content block came back" by resending the
// same request verbatim; this layer handles "only thinking, no body and no tool
// call" by appending an instruction that gets the model to continue from its
// existing thinking (resending verbatim is meaningless for this kind of stall,
// which is shaped by context rather than a transport failure). The emptiness
// check differs because the SDK goes by "did any event get yielded", and a
// thinking delta IS an event — but what the user means by "empty-response retry
// count" is "retry once more if the model didn't produce substantive output", so
// the two layers share one count to match that intent.
//
// This reads the global policy rather than a profile's override: a run may
// switch profiles mid-flight on failover, and this is the overall cap for the
// whole intent, which should not change just because the endpoint changed. The
// semantics mirror the SDK's emptyRetries(): 0 = default defaultEmptyTurnNudges;
// -1 (negative) = turn off empty-turn continuation; >0 = use that value.
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
