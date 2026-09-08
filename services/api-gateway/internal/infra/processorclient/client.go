// Package processorclient is the gateway's HTTP client for the Rust
// processor's internal API, described by
// contracts/internal-api/processor-v1.json.
//
// The client owns everything about the call that the caller should not have
// to think about: the per-attempt timeout, the bounded retries with
// exponential backoff, the retry classification, the credential, and the
// propagation of the request and correlation identifiers. It turns every
// failure into a *processing.ProcessorError carrying a gateway-side code, a
// message safe to return to a client, and whether the call was worth
// repeating.
//
// The processor's own error messages never reach a client of the gateway.
// They are written for operators of that service and can name internal
// limits; they are logged and dropped.
package processorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/observability/tracing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/processing"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// Paths of the internal API (contract 1.1.0).
const (
	processPath = "/internal/v1/process"
	healthPath  = "/internal/v1/health"
)

// MaxResponseBytes bounds a response body. A processor answering with
// something enormous must not be able to exhaust the gateway's memory; the
// limit is generous enough for the largest outcome a bounded job can
// produce.
const MaxResponseBytes = 64 << 20

// Client calls the processor.
type Client struct {
	http     *http.Client
	baseURL  string
	token    string
	cfg      config.Processor
	logger   *slog.Logger
	tracer   tracing.Tracer
	sleep    func(context.Context, time.Duration) error
	now      func() time.Time
	contract string
}

// Options tune a Client. Nil fields take safe defaults.
type Options struct {
	// HTTPClient replaces the default transport. Its own Timeout is left
	// alone: the per-attempt bound comes from the context, so that the
	// caller's deadline always wins.
	HTTPClient *http.Client
	Sleep      func(context.Context, time.Duration) error
	Now        func() time.Time
	// Tracer opens one client span per attempt; nil records none.
	Tracer tracing.Tracer
}

// New returns a client for the processor described by cfg.
func New(cfg config.Processor, logger *slog.Logger, opts Options) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Transport: http.DefaultTransport}
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		http:     httpClient,
		baseURL:  strings.TrimSuffix(cfg.BaseURL, "/"),
		token:    string(cfg.Token),
		cfg:      cfg,
		tracer:   tracing.OrNoop(opts.Tracer),
		logger:   logger,
		sleep:    sleep,
		now:      now,
		contract: cfg.ContractVersion,
	}
}

// Health is the processor's `GET /internal/v1/health` response, enough of
// it for the gateway to check that it is talking to a compatible peer.
type Health struct {
	Status           string `json:"status"`
	Service          string `json:"service"`
	Version          string `json:"version"`
	AlgorithmVersion string `json:"algorithm_version"`
	ContractVersion  string `json:"contract_version"`
	Limits           Limits `json:"limits"`
}

// Limits are the bounds the processor advertises.
type Limits struct {
	MaxJobMeasurements  int64 `json:"max_job_measurements"`
	MaxRequestBytes     int64 `json:"max_request_bytes"`
	ProcessingTimeoutMs int64 `json:"processing_timeout_ms"`
	MaxConcurrentJobs   int64 `json:"max_concurrent_jobs"`
	JobRetentionSeconds int64 `json:"job_retention_seconds"`
}

// Health reads the processor's health endpoint. It needs no credential and
// is not retried: a caller checking whether the processor is there wants an
// answer now.
func (c *Client) Health(ctx context.Context) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+healthPath, nil)
	if err != nil {
		return Health{}, c.protocolError("build the health request", err)
	}
	c.setCorrelation(ctx, req)

	res, err := c.http.Do(req)
	if err != nil {
		return Health{}, c.transportError(err)
	}
	defer drain(res)

	if res.StatusCode != http.StatusOK {
		return Health{}, c.failure(res, nil)
	}
	var health Health
	if err := decode(res, &health); err != nil {
		return Health{}, c.protocolError("read the health response", err)
	}
	return health, nil
}

// Process dispatches one job, retrying a retryable failure up to
// MaxAttempts times while the caller's deadline allows another attempt.
//
// Retrying a dispatch is safe because the job carries the identifier of a
// job this gateway created: the processor refuses a duplicate that is still
// running with a conflict, and a job that already finished left no state
// behind that a repeat would corrupt.
func (c *Client) Process(ctx context.Context, in processing.Request) (processing.Outcome, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return processing.Outcome{}, c.protocolError("encode the processing request", err)
	}

	var last error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			wait := c.backoff(attempt, last)
			if !c.timeAllows(ctx, wait) {
				break
			}
			if err := c.sleep(ctx, wait); err != nil {
				return processing.Outcome{}, c.contextError(ctx, err)
			}
		}
		outcome, err := c.attempt(ctx, body, in.Job.ID)
		if err == nil {
			if attempt > 1 {
				c.logger.InfoContext(ctx, "processor call succeeded after a retry",
					"job_id", in.Job.ID, "attempt", attempt)
			}
			return outcome, nil
		}
		last = err

		var pe *processing.ProcessorError
		if !errors.As(err, &pe) || !pe.Retryable {
			return processing.Outcome{}, err
		}
		c.logger.WarnContext(ctx, "processor call failed, may retry",
			"job_id", in.Job.ID, "attempt", attempt, "max_attempts", c.cfg.MaxAttempts,
			"code", pe.Code, "error", pe.Err)
		if ctx.Err() != nil {
			return processing.Outcome{}, c.contextError(ctx, ctx.Err())
		}
	}
	return processing.Outcome{}, last
}

// attempt performs one call. It never retries.
// host is the processor's address for a span: scheme and host only, never
// a path, so nothing derived from a request reaches the attribute.
func (c *Client) host() string {
	if u, err := url.Parse(c.baseURL); err == nil {
		return u.Host
	}
	return ""
}

func (c *Client) attempt(ctx context.Context, body []byte, jobID string) (processing.Outcome, error) {
	// Every attempt gets the same bound, and the caller's deadline always
	// wins because it is already on ctx.
	attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	// One client span per attempt, so a retried call shows in the trace as
	// what it was: several attempts, each with its own outcome, rather
	// than one long gap between the gateway's span and the processor's.
	// The span is what the processor's server span becomes a child of,
	// because the trace context injected below comes from its context.
	attemptCtx, span := c.tracer.Start(attemptCtx, "processor.process")
	defer span.End()
	span.SetAttribute("http.request.method", http.MethodPost)
	span.SetAttribute("server.address", c.host())

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.baseURL+processPath, bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		return processing.Outcome{}, c.protocolError("build the processing request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.ContentLength = int64(len(body))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	c.setCorrelation(attemptCtx, req)
	// Tell the processor what is left of this attempt, so it stops work
	// nobody is waiting for rather than running to completion.
	if budget, ok := remaining(attemptCtx, c.now()); ok {
		req.Header.Set("X-Request-Timeout-Ms", strconv.FormatInt(budget, 10))
	}

	res, err := c.http.Do(req)
	if err != nil {
		span.RecordError(err)
		return processing.Outcome{}, c.transportError(err)
	}
	defer drain(res)
	span.SetAttribute("http.response.status_code", res.StatusCode)

	if res.StatusCode != http.StatusOK {
		failure := c.failure(res, &jobID)
		span.RecordError(failure)
		return processing.Outcome{}, failure
	}
	var outcome processing.Outcome
	if err := decode(res, &outcome); err != nil {
		return processing.Outcome{}, c.protocolError("read the processing outcome", err)
	}
	if outcome.JobID != jobID {
		return processing.Outcome{}, c.protocolError("match the outcome to the job",
			fmt.Errorf("the outcome is for job %q, not %q", outcome.JobID, jobID))
	}
	return outcome, nil
}

// setCorrelation propagates the identifiers of the request being served,
// and the W3C trace context that makes the processor's work part of this
// request's trace rather than a trace of its own.
func (c *Client) setCorrelation(ctx context.Context, req *http.Request) {
	tracing.Inject(ctx, req.Header)
	if id := requestid.FromContext(ctx); requestid.Valid(id) {
		req.Header.Set(requestid.Header, id)
	}
	if id := requestid.CorrelationFromContext(ctx); requestid.Valid(id) {
		req.Header.Set(requestid.CorrelationHeader, id)
	}
}

// envelope is the processor's error body. Only the code is used: the
// message is written for operators of that service.
type envelope struct {
	Error struct {
		Code      string `json:"code"`
		Retryable *bool  `json:"retryable"`
	} `json:"error"`
}

// failure classifies a non-200 response.
func (c *Client) failure(res *http.Response, jobID *string) error {
	var body envelope
	if err := decode(res, &body); err != nil {
		// A failure whose body cannot be read is still classified by its
		// status, which is the part of the answer that is always there.
		c.logger.Warn("processor error body unreadable", "status", res.StatusCode, "error", err)
	}
	code := body.Error.Code
	pe := classify(res.StatusCode, code)
	pe.Err = fmt.Errorf("processor answered %d %s", res.StatusCode, orUnknown(code))
	if pe.Retryable {
		if after, ok := retryAfter(res); ok {
			pe.RetryAfter = after
		}
	}
	if jobID != nil {
		pe.Err = fmt.Errorf("job %s: %w", *jobID, pe.Err)
	}
	return pe
}

// Error is a *processing.ProcessorError plus the delay the processor asked
// for, which only this package acts on.
type Error struct {
	*processing.ProcessorError
	RetryAfter time.Duration
}

// Unwrap exposes the classified error underneath, so that a caller holding
// only the port's type still finds it with errors.As. Without this the
// embedded Unwrap would be promoted and would skip straight to the cause,
// and every failure would reach the service unclassified.
func (e *Error) Unwrap() error { return e.ProcessorError }

func classify(status int, code string) *Error {
	e := &Error{ProcessorError: &processing.ProcessorError{}}
	switch {
	// The processor has no capacity or is shutting down. Both pass.
	case status == http.StatusServiceUnavailable && code != "PROCESSING_CANCELLED":
		e.Code = processing.CodeProcessorUnavailable
		e.Message = "The processing service is unavailable; retry later."
		e.Kind = domain.KindUnavailable
		e.Retryable = true

	// The work was stopped on request. Repeating it would undo that.
	case status == http.StatusServiceUnavailable:
		e.Code = processing.CodeProcessorUnavailable
		e.Message = "The processing service stopped the job."
		e.Kind = domain.KindUnavailable
		e.Retryable = false

	case status == http.StatusGatewayTimeout:
		e.Code = processing.CodeProcessorTimeout
		e.Message = "The processing service did not finish in time."
		e.Kind = domain.KindTimeout
		e.Retryable = true

	case status == http.StatusInternalServerError:
		e.Code = processing.CodeProcessorProtocol
		e.Message = "The processing service failed to process the job."
		e.Kind = domain.KindUnavailable
		e.Retryable = true

	// The data cannot be processed. The client can act on this, so the code
	// distinguishes the two cases the contract names.
	case status == http.StatusUnprocessableEntity && code == "NO_VALID_MEASUREMENTS":
		e.Code = processing.CodeNoMeasurements
		e.Message = "None of the measurements in the requested range could be processed."
		e.Kind = domain.KindValidation
	case status == http.StatusUnprocessableEntity && code == "JOB_TOO_LARGE":
		e.Code = processing.CodeJobTooLarge
		e.Message = "The requested range holds more measurements than one job may carry; narrow it with from and to."
		e.Kind = domain.KindValidation
	case status == http.StatusUnprocessableEntity:
		e.Code = processing.CodeProcessorRejected
		e.Message = "The processing service could not process the job."
		e.Kind = domain.KindValidation

	// The processor is still running an earlier attempt at this job, which
	// happens when an attempt timed out here but not there. Waiting is the
	// right response: the earlier run will finish and release the id.
	case status == http.StatusConflict && code == "JOB_ALREADY_RUNNING":
		e.Code = processing.CodeProcessorBusy
		e.Message = "The processing service is still working on an earlier attempt at this job; retry later."
		e.Kind = domain.KindUnavailable
		e.Retryable = true

	// Everything else means the two services disagree: a credential the
	// processor will not accept, a request it cannot parse, a version it
	// does not implement. None of it is the client's fault and none of it
	// is safe to describe to them.
	default:
		e.Code = processing.CodeProcessorProtocol
		e.Message = "The processing service could not be used."
		e.Kind = domain.KindUnavailable
	}
	return e
}

// transportError classifies a call that never got an answer: a refused
// connection, a reset, a DNS failure, or the attempt's own deadline. All of
// them are worth repeating, except a caller that has given up.
func (c *Client) transportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return &processing.ProcessorError{
			Code:    processing.CodeProcessorTimeout,
			Message: "The request was cancelled before the processing service answered.",
			Kind:    domain.KindTimeout,
			Err:     err,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{ProcessorError: &processing.ProcessorError{
			Code:      processing.CodeProcessorTimeout,
			Message:   "The processing service did not answer in time.",
			Kind:      domain.KindTimeout,
			Retryable: true,
			Err:       err,
		}}
	}
	return &Error{ProcessorError: &processing.ProcessorError{
		Code:      processing.CodeProcessorUnavailable,
		Message:   "The processing service could not be reached; retry later.",
		Kind:      domain.KindUnavailable,
		Retryable: true,
		Err:       err,
	}}
}

// protocolError reports an answer this gateway cannot use. It is never
// retried: repeating a call that produced nonsense produces nonsense.
func (c *Client) protocolError(what string, err error) error {
	return &processing.ProcessorError{
		Code:    processing.CodeProcessorProtocol,
		Message: "The processing service returned something this gateway cannot use.",
		Kind:    domain.KindUnavailable,
		Err:     fmt.Errorf("could not %s: %w", what, err),
	}
}

func (c *Client) contextError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &processing.ProcessorError{
			Code:    processing.CodeProcessorTimeout,
			Message: "The processing service did not answer in time.",
			Kind:    domain.KindTimeout,
			Err:     err,
		}
	}
	return &processing.ProcessorError{
		Code:    processing.CodeProcessorTimeout,
		Message: "The request was cancelled before the processing service answered.",
		Kind:    domain.KindTimeout,
		Err:     err,
	}
}

// backoff is the delay before `attempt`: exponential from the configured
// base, capped, unless the processor asked for a specific delay.
func (c *Client) backoff(attempt int, last error) time.Duration {
	var e *Error
	if errors.As(last, &e) && e.RetryAfter > 0 {
		return min(e.RetryAfter, c.cfg.MaxBackoff)
	}
	// attempt is at least 2 here, so the first wait is the base delay.
	factor := math.Pow(2, float64(attempt-2))
	wait := time.Duration(float64(c.cfg.Backoff) * factor)
	return min(wait, c.cfg.MaxBackoff)
}

// timeAllows reports whether there is time for a wait plus another attempt.
// Sleeping into a deadline only to be cancelled wastes the caller's time.
func (c *Client) timeAllows(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return c.now().Add(wait).Add(minimumAttempt).Before(deadline)
}

// minimumAttempt is the least time in which an attempt could plausibly
// succeed. Below it, retrying is a waste of the caller's remaining budget.
const minimumAttempt = 50 * time.Millisecond

// remaining is the milliseconds left on ctx, if it has a deadline, clamped
// to the range the contract accepts.
func remaining(ctx context.Context, now time.Time) (int64, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	ms := deadline.Sub(now).Milliseconds()
	switch {
	case ms < 1:
		return 1, true
	case ms > 3_600_000:
		return 3_600_000, true
	default:
		return ms, true
	}
}

func retryAfter(res *http.Response) (time.Duration, bool) {
	raw := res.Header.Get("Retry-After")
	if raw == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// decode reads a bounded JSON body.
func decode(res *http.Response, dst any) error {
	body := io.LimitReader(res.Body, MaxResponseBytes+1)
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if len(raw) > MaxResponseBytes {
		return fmt.Errorf("the response exceeds %d bytes", MaxResponseBytes)
	}
	if len(raw) == 0 {
		return errors.New("the response has no body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Unknown fields are ignored on purpose. Adding a response field is a
	// backward-compatible change under the contract's own versioning rules,
	// so refusing one here would break this gateway on a release the
	// processor was entitled to make. Drift is caught at build time by the
	// contract tests instead. The body is never logged: it holds patient
	// data.
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if dec.More() {
		return errors.New("the response holds more than one JSON value")
	}
	return nil
}

// drain reads and closes the body so the connection can be reused.
func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	_ = res.Body.Close()
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func orUnknown(code string) string {
	if code == "" {
		return "(no code)"
	}
	return code
}

// ValidateBaseURL reports whether raw is a usable processor address.
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be an http or https URL")
	}
	if u.Host == "" {
		return errors.New("must name a host")
	}
	return nil
}
