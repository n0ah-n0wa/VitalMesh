// Package request reads and validates client input at the HTTP boundary:
// JSON bodies and pagination parameters. Every failure is a *domain.Error
// with a client-safe message.
package request

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/pagination"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/validate"
)

// Error codes produced by this package.
const (
	CodeInvalidJSON      = "INVALID_JSON"
	CodeEmptyBody        = "EMPTY_BODY"
	CodeBodyTooLarge     = "REQUEST_BODY_TOO_LARGE"
	CodeUnsupportedMedia = "UNSUPPORTED_MEDIA_TYPE"
)

const jsonMediaType = "application/json"

// ErrBodyTooLarge is the error for a body exceeding limit bytes. It is shared
// with the body-limit middleware so both paths report the same failure.
func ErrBodyTooLarge(limit int64) *domain.Error {
	return domain.New(domain.KindTooLarge, CodeBodyTooLarge,
		fmt.Sprintf("The request body must not exceed %d bytes.", limit))
}

// DecodeJSON reads the request body into dst. It requires a JSON content
// type, rejects unknown fields, and rejects bodies that contain more than one
// JSON value. The body size is bounded by the body-limit middleware.
func DecodeJSON(r *http.Request, dst any) error {
	if err := requireJSON(r); err != nil {
		return err
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return classifyDecodeError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.New(domain.KindInvalid, CodeInvalidJSON, "The request body must contain a single JSON value.")
	}
	return nil
}

func requireJSON(r *http.Request) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != jsonMediaType {
		return domain.New(domain.KindUnsupportedMedia, CodeUnsupportedMedia, "The Content-Type must be application/json.")
	}
	return nil
}

func classifyDecodeError(err error) error {
	var (
		tooLarge  *http.MaxBytesError
		syntax    *json.SyntaxError
		wrongType *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &tooLarge):
		return ErrBodyTooLarge(tooLarge.Limit)
	case errors.Is(err, io.EOF):
		return domain.New(domain.KindInvalid, CodeEmptyBody, "The request body must not be empty.")
	case errors.As(err, &syntax):
		return domain.New(domain.KindInvalid, CodeInvalidJSON,
			fmt.Sprintf("The request body contains malformed JSON at position %d.", syntax.Offset))
	case errors.Is(err, io.ErrUnexpectedEOF):
		return domain.New(domain.KindInvalid, CodeInvalidJSON, "The request body contains malformed JSON.")
	case errors.As(err, &wrongType):
		field := wrongType.Field
		if field == "" {
			field = "body"
		}
		e := domain.New(domain.KindInvalid, CodeInvalidJSON, "The request body has a value of the wrong type.")
		e.Details = []domain.FieldError{{Field: field, Message: "has the wrong type"}}
		return e
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json has no typed error for this case.
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		e := domain.New(domain.KindInvalid, CodeInvalidJSON, "The request body contains an unknown field.")
		e.Details = []domain.FieldError{{Field: field, Message: "is not a known field"}}
		return e
	default:
		// Anything else (for example decoding into a non-pointer) is a
		// programming error and is reported as internal.
		return err
	}
}

// Pagination parses the limit and cursor query parameters. The cursor is
// passed through untouched; repositories decode it with pagination.DecodeCursor.
func Pagination(r *http.Request, limits pagination.Limits) (pagination.Request, error) {
	q := r.URL.Query()
	req := pagination.Request{Limit: limits.Default, Cursor: q.Get("cursor")}

	var v validate.Validator
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			v.Add("limit", "must be an integer")
		} else {
			req.Limit = n
			validate.InRange(&v, "limit", n, 1, limits.Max)
		}
	}
	v.MaxLength("cursor", req.Cursor, pagination.MaxCursorLength)

	if err := v.Err(); err != nil {
		return pagination.Request{}, err
	}
	return req, nil
}
