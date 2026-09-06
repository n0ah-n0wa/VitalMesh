package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

const userColumns = `id, email, password_hash, role, status, created_at, updated_at`

// Users persists accounts.
type Users struct {
	db DB
}

// NewUsers returns a repository bound to db, which may be a pool or a
// transaction.
func NewUsers(db DB) *Users { return &Users{db: db} }

// UserCursor is the keyset position after which List continues.
type UserCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        uuid.UUID `json:"id"`
}

// Create inserts a user. Email must already be lower case.
func (r *Users) Create(ctx context.Context, email, passwordHash string, role domain.Role) (domain.User, error) {
	rows, err := r.db.Query(ctx, `
		INSERT INTO users (email, password_hash, role)
		VALUES ($1, $2, $3)
		RETURNING `+userColumns,
		email, passwordHash, role)
	return collectOne[domain.User](rows, err)
}

// GetByID returns the user or a not-found error.
func (r *Users) GetByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	rows, err := r.db.Query(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
	return collectOne[domain.User](rows, err)
}

// GetByEmail returns the user with the given (lower case) email.
func (r *Users) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	rows, err := r.db.Query(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email)
	return collectOne[domain.User](rows, err)
}

// List returns up to limit users in creation order, starting after the
// cursor when one is given.
func (r *Users) List(ctx context.Context, after *UserCursor, limit int) ([]domain.User, error) {
	if after == nil {
		rows, err := r.db.Query(ctx, `
			SELECT `+userColumns+` FROM users
			ORDER BY created_at, id
			LIMIT $1`, limit)
		return collectAll[domain.User](rows, err)
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+userColumns+` FROM users
		WHERE (created_at, id) > ($1, $2)
		ORDER BY created_at, id
		LIMIT $3`, after.CreatedAt, after.ID, limit)
	return collectAll[domain.User](rows, err)
}

// UpdateRole changes a user's role and returns the updated row.
func (r *Users) UpdateRole(ctx context.Context, id uuid.UUID, role domain.Role) (domain.User, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE users SET role = $2 WHERE id = $1
		RETURNING `+userColumns,
		id, role)
	return collectOne[domain.User](rows, err)
}

// SetStatus enables or disables a user and returns the updated row.
func (r *Users) SetStatus(ctx context.Context, id uuid.UUID, status domain.UserStatus) (domain.User, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE users SET status = $2 WHERE id = $1
		RETURNING `+userColumns,
		id, status)
	return collectOne[domain.User](rows, err)
}

// collectOne scans exactly one row into T, mapping driver errors.
func collectOne[T any](rows pgx.Rows, err error) (T, error) {
	var zero T
	if err != nil {
		return zero, mapError(err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[T])
	if err != nil {
		return zero, mapError(err)
	}
	return row, nil
}

// collectAll scans every row into a slice of T, mapping driver errors.
func collectAll[T any](rows pgx.Rows, err error) ([]T, error) {
	if err != nil {
		return nil, mapError(err)
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[T])
	if err != nil {
		return nil, mapError(err)
	}
	return items, nil
}

// collectBatch reads one row from each of n queued batch statements. The
// batch is closed in every case; when a statement fails the remaining
// results are discarded and the error is returned.
func collectBatch[T any](results pgx.BatchResults, n int) ([]T, error) {
	out := make([]T, 0, n)
	for range n {
		rows, err := results.Query()
		item, err := collectOne[T](rows, err)
		if err != nil {
			_ = results.Close()
			return nil, err
		}
		out = append(out, item)
	}
	if err := results.Close(); err != nil {
		return nil, mapError(err)
	}
	return out, nil
}
