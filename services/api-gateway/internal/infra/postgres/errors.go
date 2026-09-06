package postgres

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// PostgreSQL error codes this layer classifies.
const (
	codeStringTooLong         = "22001"
	codeNumericOutOfRange     = "22003"
	codeInvalidTextRepr       = "22P02"
	codeNotNullViolation      = "23502"
	codeForeignKeyViolation   = "23503"
	codeUniqueViolation       = "23505"
	codeCheckViolation        = "23514"
	codeSerializationFailure  = "40001"
	codeDeadlockDetected      = "40P01"
	codeInsufficientPrivilege = "42501"
)

// mapError classifies a driver error as a domain error where the cause is
// a well-defined integrity or input rule, so callers can respond without
// knowing PostgreSQL. For constraint violations the error code is the
// violated constraint's name in upper case. Messages never include row data.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Wrap(err, domain.KindNotFound, "NOT_FOUND", "The requested resource does not exist.")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	code := constraintCode(pgErr)
	switch pgErr.Code {
	case codeUniqueViolation:
		return domain.Wrap(err, domain.KindConflict, code, "The resource already exists.")
	case codeForeignKeyViolation:
		return domain.Wrap(err, domain.KindValidation, code, "A referenced resource does not exist.")
	case codeCheckViolation:
		return domain.Wrap(err, domain.KindValidation, code, "The value violates a data constraint.")
	case codeNotNullViolation:
		return domain.Wrap(err, domain.KindValidation, "NOT_NULL_VIOLATION", "A required value is missing.")
	case codeStringTooLong:
		return domain.Wrap(err, domain.KindValidation, "VALUE_TOO_LONG", "A value exceeds its maximum length.")
	case codeNumericOutOfRange:
		return domain.Wrap(err, domain.KindValidation, "VALUE_OUT_OF_RANGE", "A numeric value is out of range.")
	case codeInvalidTextRepr:
		return domain.Wrap(err, domain.KindInvalid, "INVALID_INPUT", "A value has an invalid format.")
	case codeInsufficientPrivilege:
		return domain.Wrap(err, domain.KindConflict, "APPEND_ONLY", "The record cannot be changed.")
	case codeSerializationFailure, codeDeadlockDetected:
		return domain.Wrap(err, domain.KindUnavailable, "TRANSACTION_CONFLICT", "The operation conflicted with another; retry.")
	default:
		return err
	}
}

func constraintCode(pgErr *pgconn.PgError) string {
	if pgErr.ConstraintName == "" {
		return "CONSTRAINT_VIOLATION"
	}
	return strings.ToUpper(pgErr.ConstraintName)
}
