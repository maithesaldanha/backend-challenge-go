package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"strings"

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
	var networkError net.Error
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) {
		return errors.Join(ports.ErrUnavailable, err)
	}
	var stateError interface{ SQLState() string }
	if errors.As(err, &stateError) {
		switch state := stateError.SQLState(); {
		case state == "23505":
			return errors.Join(ports.ErrConflict, err)
		case strings.HasPrefix(state, "08"), state == "57P01", state == "53300":
			return errors.Join(ports.ErrUnavailable, err)
		}
	}
	return err
}
