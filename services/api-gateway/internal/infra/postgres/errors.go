package postgres

import (
	"errors"
	"io"
	"net"
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
	// The server is there but cannot take the work, or is going away.
	codeTooManyConnections = "53300"
	codeAdminShutdown      = "57P01"
	codeCrashShutdown      = "57P02"
	codeCannotConnectNow   = "57P03"
	// Class 08: connection exceptions.
	classConnectionException = "08"
)

// unavailable is the answer for a database that cannot be reached: the
// service is degraded rather than broken, the client may retry, and
// readiness is what reports the outage (docs/FAILURE_MODES.md). The
// message names no host, port or driver.
func unavailable(err error) error {
	return domain.Wrap(err, domain.KindUnavailable, "DATABASE_UNAVAILABLE", "The database is unavailable; retry later.")
}

// isConnectivity reports whether err is the database being unreachable
// rather than anything about the query: a failed connection attempt, a
// network error, a connection that died mid-conversation, or the pool's
// own dial timeout.
func isConnectivity(err error) bool {
	var connectErr *pgconn.ConnectError
	var netErr net.Error
	switch {
	case errors.As(err, &connectErr), errors.As(err, &netErr):
		return true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case pgconn.SafeToRetry(err):
		// The request never reached the server, so nothing ran.
		return true
	}
	// pgconn reports a connection torn down under a query in words only.
	msg := err.Error()
	return strings.Contains(msg, "conn closed") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe")
}

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
		if isConnectivity(err) {
			return unavailable(err)
		}
		return err
	}
	if strings.HasPrefix(pgErr.Code, classConnectionException) {
		return unavailable(err)
	}
	code := constraintCode(pgErr)
	switch pgErr.Code {
	case codeTooManyConnections, codeAdminShutdown, codeCrashShutdown, codeCannotConnectNow:
		return unavailable(err)
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
