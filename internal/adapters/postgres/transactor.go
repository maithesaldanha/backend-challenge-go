package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
)

type Transactor struct {
	db *sql.DB
}

func NewTransactor(db *sql.DB) (*Transactor, error) {
	if db == nil {
		return nil, errors.New("postgres database is required")
	}
	return &Transactor{db: db}, nil
}

func (t *Transactor) WithinTransaction(ctx context.Context, fn func(context.Context, ports.UnitOfWork) error) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback()

	unit := newUnitOfWork(tx)
	if err := fn(ctx, unit); err != nil {
		return mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return mapError(err)
	}
	return nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var stateError interface{ SQLState() string }
	if errors.As(err, &stateError) && stateError.SQLState() == "23505" {
		return errors.Join(ports.ErrConflict, err)
	}
	return err
}
