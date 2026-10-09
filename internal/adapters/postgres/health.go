package postgres

import (
	"context"
	"database/sql"
	"errors"
)

type HealthChecker struct {
	db *sql.DB
}

func NewHealthChecker(db *sql.DB) (*HealthChecker, error) {
	if db == nil {
		return nil, errors.New("postgres database is required")
	}
	return &HealthChecker{db: db}, nil
}

func (c *HealthChecker) Name() string { return "postgres" }

func (c *HealthChecker) Check(ctx context.Context) error {
	return c.db.PingContext(ctx)
}
