package synth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// Loader pushes a fixture through a gateway's public API, the way any
// client would: it signs in, creates the patients and stores the readings
// in batches, optionally running one processing job per patient. Every
// write carries an Idempotency-Key derived from the fixture, so loading the
// same fixture twice stores nothing twice, and it only ever creates: no
// call it makes deletes or changes anything that was there before.
type Loader struct {
	// Client and BaseURL name the gateway. Call [CheckTarget] first.
	Client  *http.Client
	BaseURL string
	// BatchSize is how many readings go into one batch request; the API
	// accepts at most 1000 by default.
	BatchSize int
	// Jobs runs one processing job per patient over the fixture's types
	// and time range once its readings are stored.
	Jobs bool
	// Windows and Percentiles parametrise those jobs.
	Windows     []string
	Percentiles []int
	// Log receives one line per patient and per problem; nil discards.
	Log io.Writer
	// Sleep is what a rate-limited or busy answer waits with; nil means
	// time.Sleep. Tests replace it.
	Sleep func(time.Duration)
	// MaxRetries bounds the retries of one request after 429, 409
	// IDEMPOTENCY_IN_PROGRESS, 502, 503 and 504.
	MaxRetries int

	token string
}

// Report is what a load did. Existing counts are records the gateway
// already had (an idempotent replay or a conflict), which a second run of
// the same fixture produces in full.
type Report struct {
	Patients     struct{ Created, Existing int } `json:"patients"`
	Measurements struct {
		Stored, Existing, Batches int
	} `json:"measurements"`
	Jobs struct{ Completed, Failed int } `json:"jobs"`
}

func (l *Loader) logf(format string, args ...any) {
	if l.Log != nil {
		fmt.Fprintf(l.Log, format+"\n", args...)
	}
}

func (l *Loader) sleep(d time.Duration) {
	if l.Sleep != nil {
		l.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Login signs in and keeps the token for the calls that follow. The
// password is sent to the gateway and nowhere else; it is never logged.
func (l *Loader) Login(ctx context.Context, email, password string) error {
	body, _ := json.Marshal(map[string]string{"email": email, "password": password})
	status, resp, _, err := l.do(ctx, http.MethodPost, "/api/v1/auth/login", body, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("sign in as %s: %s", email, apiError(status, resp))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || out.AccessToken == "" {
		return fmt.Errorf("sign in as %s: the gateway returned no token", email)
	}
	l.token = out.AccessToken
	return nil
}

// Load reads the fixture in dir (checking its manifest) and stores it.
func (l *Loader) Load(ctx context.Context, dir string) (Report, error) {
	var r Report
	if l.token == "" {
		return r, errors.New("not signed in")
	}
	if l.BatchSize <= 0 {
		l.BatchSize = 500
	}
	m, err := ReadManifest(dir)
	if err != nil {
		return r, err
	}
	patients, err := ReadPatients(dir)
	if err != nil {
		return r, err
	}

	ids := make(map[string]uuid.UUID, len(patients))
	for _, p := range patients {
		id, created, err := l.createPatient(ctx, p)
		if err != nil {
			return r, fmt.Errorf("patient %s: %w", p.ExternalReference, err)
		}
		ids[p.ExternalReference] = id
		if created {
			r.Patients.Created++
		} else {
			r.Patients.Existing++
		}
	}
	l.logf("patients: %d created, %d already there", r.Patients.Created, r.Patients.Existing)

	// Readings are contiguous per patient in the fixture, so a batch never
	// spans two patients and the key can be numbered per patient.
	var (
		current string
		items   []batchItem
		ordinal int
	)
	flush := func() error {
		if len(items) == 0 {
			return nil
		}
		key := fmt.Sprintf("synth:batch:%s:%d:%d", current, l.BatchSize, ordinal)
		stored, existing, err := l.storeBatch(ctx, items, key)
		if err != nil {
			return fmt.Errorf("patient %s batch %d: %w", current, ordinal, err)
		}
		r.Measurements.Stored += stored
		r.Measurements.Existing += existing
		r.Measurements.Batches++
		ordinal++
		items = items[:0]
		return nil
	}
	err = EachMeasurement(dir, func(mm Measurement) error {
		if mm.PatientReference != current {
			if err := flush(); err != nil {
				return err
			}
			if current != "" {
				l.logf("patient %s: readings stored", current)
			}
			current, ordinal = mm.PatientReference, 0
		}
		id, ok := ids[mm.PatientReference]
		if !ok {
			return fmt.Errorf("reading names patient %s, which the fixture does not define", mm.PatientReference)
		}
		items = append(items, batchItem{
			PatientID: id.String(), Type: string(mm.Type), Value: mm.Value, Unit: mm.Unit,
			RecordedAt: mm.RecordedAt.UTC().Format(time.RFC3339Nano), Source: mm.Source, Metadata: mm.Metadata,
		})
		if len(items) >= l.BatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	if err := flush(); err != nil {
		return r, err
	}
	if current != "" {
		l.logf("patient %s: readings stored", current)
	}
	l.logf("measurements: %d stored, %d already there, in %d batches",
		r.Measurements.Stored, r.Measurements.Existing, r.Measurements.Batches)

	if l.Jobs {
		types := make([]string, 0, len(m.Spec.Streams))
		for _, t := range m.Spec.Types() {
			types = append(types, string(t))
		}
		for _, p := range patients {
			ok, err := l.runJob(ctx, ids[p.ExternalReference], p.ExternalReference, types, m.Spec.Time)
			if err != nil {
				return r, fmt.Errorf("patient %s job: %w", p.ExternalReference, err)
			}
			if ok {
				r.Jobs.Completed++
			} else {
				r.Jobs.Failed++
			}
		}
		l.logf("jobs: %d completed, %d failed", r.Jobs.Completed, r.Jobs.Failed)
	}
	return r, nil
}

type batchItem struct {
	PatientID  string          `json:"patient_id"`
	Type       string          `json:"type"`
	Value      float64         `json:"value"`
	Unit       string          `json:"unit"`
	RecordedAt string          `json:"recorded_at"`
	Source     string          `json:"source"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

// createPatient creates p, or finds it when a patient with its reference
// exists already. It reports whether it created.
func (l *Loader) createPatient(ctx context.Context, p Patient) (uuid.UUID, bool, error) {
	body, _ := json.Marshal(p)
	status, resp, header, err := l.do(ctx, http.MethodPost, "/api/v1/patients", body, "synth:patient:"+p.ExternalReference)
	if err != nil {
		return uuid.Nil, false, err
	}
	switch {
	case status == http.StatusCreated:
		var out struct {
			ID uuid.UUID `json:"id"`
		}
		if err := json.Unmarshal(resp, &out); err != nil || out.ID == uuid.Nil {
			return uuid.Nil, false, errors.New("the gateway returned no patient id")
		}
		return out.ID, header.Get("Idempotency-Replayed") == "", nil
	case status == http.StatusConflict && apiCode(resp) == "PATIENT_ALREADY_EXISTS":
		id, err := l.findPatient(ctx, p.ExternalReference)
		return id, false, err
	default:
		return uuid.Nil, false, errors.New(apiError(status, resp))
	}
}

// findPatient pages through the patient list for an external reference.
// The list has no filter by reference, so this is a walk; it only happens
// when a patient exists but the idempotency record of its creation is gone.
func (l *Loader) findPatient(ctx context.Context, ref string) (uuid.UUID, error) {
	cursor := ""
	for {
		path := "/api/v1/patients?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		status, resp, _, err := l.do(ctx, http.MethodGet, path, nil, "")
		if err != nil {
			return uuid.Nil, err
		}
		if status != http.StatusOK {
			return uuid.Nil, errors.New(apiError(status, resp))
		}
		var page struct {
			Items []struct {
				ID                uuid.UUID `json:"id"`
				ExternalReference string    `json:"external_reference"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
			HasMore    bool    `json:"has_more"`
		}
		if err := json.Unmarshal(resp, &page); err != nil {
			return uuid.Nil, fmt.Errorf("list patients: %w", err)
		}
		for _, it := range page.Items {
			if it.ExternalReference == ref {
				return it.ID, nil
			}
		}
		if !page.HasMore || page.NextCursor == nil || *page.NextCursor == "" {
			return uuid.Nil, fmt.Errorf("%s exists (the gateway says so) but is not in the patient list", ref)
		}
		cursor = *page.NextCursor
	}
}

// storeBatch stores items under key. A batch is all-or-nothing, so when
// the gateway reports that one reading already exists the items are stored
// one by one, and the ones that exist are counted as such.
func (l *Loader) storeBatch(ctx context.Context, items []batchItem, key string) (stored, existing int, err error) {
	body, _ := json.Marshal(map[string]any{"items": items})
	status, resp, header, err := l.do(ctx, http.MethodPost, "/api/v1/measurements/batch", body, key)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case status == http.StatusCreated:
		if header.Get("Idempotency-Replayed") != "" {
			return 0, len(items), nil
		}
		return len(items), 0, nil
	case status == http.StatusConflict && apiCode(resp) == "MEASUREMENT_ALREADY_EXISTS":
		for i, it := range items {
			one, _ := json.Marshal(it)
			status, resp, header, err := l.do(ctx, http.MethodPost, "/api/v1/measurements", one, fmt.Sprintf("%s:item:%d", key, i))
			if err != nil {
				return stored, existing, err
			}
			switch {
			case status == http.StatusCreated && header.Get("Idempotency-Replayed") == "":
				stored++
			case status == http.StatusCreated, status == http.StatusConflict && apiCode(resp) == "MEASUREMENT_ALREADY_EXISTS":
				existing++
			default:
				return stored, existing, fmt.Errorf("item %d: %s", i, apiError(status, resp))
			}
		}
		return stored, existing, nil
	default:
		return 0, 0, errors.New(apiError(status, resp))
	}
}

// runJob asks for one processing job over the patient's fixture readings
// and reports whether it completed. A refused or failed job is logged and
// counted, not fatal: the readings are stored either way.
func (l *Loader) runJob(ctx context.Context, id uuid.UUID, ref string, types []string, t TimeSpec) (bool, error) {
	windows := l.Windows
	if len(windows) == 0 {
		windows = []string{"1h", "24h"}
	}
	percentiles := l.Percentiles
	if len(percentiles) == 0 {
		percentiles = []int{50, 95}
	}
	req := map[string]any{
		"patient_id": id.String(), "measurement_types": types, "windows": windows, "percentiles": percentiles,
		"from": t.From.UTC().Format(time.RFC3339), "to": t.To.UTC().Format(time.RFC3339),
	}
	body, _ := json.Marshal(req)
	// Keys may hold only [A-Za-z0-9._:-], so the windows are joined with a dot.
	key := fmt.Sprintf("synth:job:%s:%s", ref, strings.Join(windows, "."))
	status, resp, _, err := l.do(ctx, http.MethodPost, "/api/v1/processing/jobs", body, key)
	if err != nil {
		return false, err
	}
	if status == http.StatusCreated {
		var out struct {
			ID     uuid.UUID `json:"id"`
			Status string    `json:"status"`
		}
		_ = json.Unmarshal(resp, &out)
		l.logf("patient %s: job %s %s", ref, out.ID, out.Status)
		return out.Status == string(domain.JobCompleted), nil
	}
	l.logf("patient %s: job refused: %s", ref, apiError(status, resp))
	return false, nil
}

// do performs one request with the token, retrying what is worth
// retrying: 429 and 409 IDEMPOTENCY_IN_PROGRESS after Retry-After, and
// 502/503/504 or a transport failure after a growing pause.
func (l *Loader) do(ctx context.Context, method, path string, body []byte, idempotencyKey string) (int, []byte, http.Header, error) {
	retries := l.MaxRetries
	if retries <= 0 {
		retries = 5
	}
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		status, resp, header, err := l.once(ctx, method, path, body, idempotencyKey)
		retry := false
		var wait time.Duration
		switch {
		case err != nil:
			retry, wait = true, backoff
		case status == http.StatusTooManyRequests,
			status == http.StatusConflict && apiCode(resp) == "IDEMPOTENCY_IN_PROGRESS":
			retry, wait = true, retryAfter(header, backoff)
		case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
			retry, wait = true, backoff
		}
		if !retry {
			return status, resp, header, nil
		}
		if attempt >= retries {
			if err != nil {
				return 0, nil, nil, fmt.Errorf("%s %s: %w (after %d attempts)", method, path, err, attempt+1)
			}
			return status, resp, header, nil
		}
		l.logf("%s %s: %s; retrying in %s", method, path, describe(status, resp, err), wait)
		select {
		case <-ctx.Done():
			return 0, nil, nil, ctx.Err()
		default:
		}
		l.sleep(wait)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (l *Loader) once(ctx context.Context, method, path string, body []byte, idempotencyKey string) (int, []byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(l.BaseURL, "/")+path, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vitalmesh-synth/"+Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if l.token != "" {
		req.Header.Set("Authorization", "Bearer "+l.token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, data, resp.Header, nil
}

func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, time.Minute)
	}
	return fallback
}

// apiCode is the error code of an error envelope, or "".
func apiCode(resp []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(resp, &e)
	return e.Error.Code
}

// apiError renders an error answer for a message: the code and message of
// the envelope with its field details, never the raw body (which could be
// anything from a proxy).
func apiError(status int, resp []byte) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(resp, &e) != nil || e.Error.Code == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d %s: %s", status, e.Error.Code, e.Error.Message)
	for i, d := range e.Error.Details {
		if i == 5 {
			fmt.Fprintf(&b, "; and %d more", len(e.Error.Details)-5)
			break
		}
		fmt.Fprintf(&b, "; %s %s", d.Field, d.Message)
	}
	return b.String()
}

func describe(status int, resp []byte, err error) string {
	if err != nil {
		return err.Error()
	}
	return apiError(status, resp)
}
