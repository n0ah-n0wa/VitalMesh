package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/metrics"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
)

// queryTracer times every statement the pool sends and records it as
// database latency.
//
// It hooks pgx rather than each repository, so a statement added later is
// measured without anyone remembering to measure it. The only label taken
// from the statement is its leading keyword: SQL carries values, and a
// statement used as a label would be both unbounded and a disclosure
// (SPECIFICATIONS.md section 41).
type queryTracer struct {
	recorder metrics.Recorder
	tracer   tracing.Tracer
}

// startedAtKey carries the start time from the begin hook to the end hook.
// pgx gives no other channel between them.
type startedAtKey struct{}

func (t queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	verb := metrics.Statement(data.SQL)
	// One span per statement, named by its verb. The statement itself is
	// never an attribute: SQL carries values, and a span is shipped to a
	// backend that is not the record's custodian (SPECIFICATIONS.md
	// sections 40 and 41).
	ctx, span := t.tracer.Start(ctx, "postgresql."+verb)
	span.SetAttribute("db.system", "postgresql")
	span.SetAttribute("db.operation.name", verb)
	return context.WithValue(ctx, startedAtKey{}, traced{
		at:   time.Now(),
		verb: verb,
		span: span,
	})
}

func (t queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	started, ok := ctx.Value(startedAtKey{}).(traced)
	if !ok {
		return
	}
	t.recorder.Database(started.verb, statementOutcome(data.Err), time.Since(started.at))
	if started.span != nil {
		if statementOutcome(data.Err) == metrics.OutcomeError {
			started.span.RecordError(data.Err)
		}
		started.span.End()
	}
}

type traced struct {
	at   time.Time
	verb string
	span tracing.Span
}

// statementOutcome classifies a statement's end. A cancelled statement is
// not a database fault and is counted apart, so a client that hangs up
// cannot look like a failing database.
func statementOutcome(err error) metrics.Outcome {
	switch {
	case err == nil:
		return metrics.OutcomeOK
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return metrics.OutcomeCancelled
	case errors.Is(err, pgx.ErrNoRows):
		// A statement that matched nothing ran correctly; whether that is a
		// problem is the caller's question, not the database's.
		return metrics.OutcomeOK
	default:
		return metrics.OutcomeError
	}
}
