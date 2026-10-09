package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/fx"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Config struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
}

var ErrInvalidConfig = errors.New("invalid postgres configuration")

func Module(config Config) fx.Option {
	return fx.Module("postgres",
		fx.Supply(config),
		fx.Provide(NewDB, NewTransactor),
		fx.Invoke(RegisterLifecycle),
	)
}

func NewDB(config Config) (*sql.DB, error) {
	var err error
	config, err = config.withDefaults()
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("pgx", config.DSN)
	if err != nil {
		return nil, fmt.Errorf("open postgres database: %w", err)
	}
	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)
	return db, nil
}

func RegisterLifecycle(lifecycle fx.Lifecycle, db *sql.DB, config Config) {
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			connectTimeout := config.ConnectTimeout
			if connectTimeout == 0 {
				connectTimeout = 5 * time.Second
			}
			connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()
			if err := db.PingContext(connectCtx); err != nil {
				_ = db.Close()
				return fmt.Errorf("connect to postgres: %w", err)
			}
			return nil
		},
		OnStop: func(context.Context) error {
			if err := db.Close(); err != nil {
				return fmt.Errorf("close postgres database: %w", err)
			}
			return nil
		},
	})
}

func (c Config) withDefaults() (Config, error) {
	if strings.TrimSpace(c.DSN) == "" || c.MaxOpenConns < 0 || c.MaxIdleConns < 0 ||
		c.ConnMaxLifetime < 0 || c.ConnectTimeout < 0 {
		return Config{}, ErrInvalidConfig
	}
	if c.MaxOpenConns == 0 {
		c.MaxOpenConns = 10
	}
	if c.MaxIdleConns == 0 {
		c.MaxIdleConns = 5
		if c.MaxIdleConns > c.MaxOpenConns {
			c.MaxIdleConns = c.MaxOpenConns
		}
	}
	if c.MaxIdleConns > c.MaxOpenConns {
		return Config{}, ErrInvalidConfig
	}
	if c.ConnMaxLifetime == 0 {
		c.ConnMaxLifetime = 30 * time.Minute
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 5 * time.Second
	}
	return c, nil
}
