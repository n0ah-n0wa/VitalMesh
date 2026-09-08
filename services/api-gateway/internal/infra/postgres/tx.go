package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WithTx runs fn inside a transaction and commits when it returns nil. Any
// error from fn, or a panic, rolls the transaction back. Keep fn short and
// free of external calls: the transaction holds locks until it ends.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) (err error) {
	return withTxOptions(ctx, pool, pgx.TxOptions{}, fn)
}

// WithSnapshot runs fn inside a read-only repeatable-read transaction, so
// every query inside it sees one instant of the database however many
// queries it makes. Use it when several reads must agree with each other;
// it takes no row locks, so it does not block writers.
func WithSnapshot(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	return withTxOptions(ctx, pool, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	}, fn)
}

func withTxOptions(ctx context.Context, pool *pgxpool.Pool, opts pgx.TxOptions, fn func(tx pgx.Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return mapError(fmt.Errorf("begin transaction: %w", err))
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return mapError(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}
