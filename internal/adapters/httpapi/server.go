package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"go.uber.org/fx"
)

var ErrInvalidServerConfig = errors.New("invalid http server configuration")

type ServerConfig struct {
	Address           string
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int
}

type Server struct {
	httpServer *http.Server
	config     ServerConfig
	listener   net.Listener
}

func Module(config ServerConfig) fx.Option {
	return fx.Module("http-api",
		fx.Supply(config),
		fx.Provide(NewHandler, NewServer),
		fx.Invoke(func(*Server) {}),
	)
}

func NewServer(lifecycle fx.Lifecycle, config ServerConfig, handler *Handler) (*Server, error) {
	if strings.TrimSpace(config.Address) == "" || config.ReadTimeout < 0 || config.ReadHeaderTimeout < 0 ||
		config.WriteTimeout < 0 || config.IdleTimeout < 0 ||
		config.ShutdownTimeout < 0 || config.MaxHeaderBytes < 0 {
		return nil, ErrInvalidServerConfig
	}
	if config.ReadHeaderTimeout == 0 {
		config.ReadHeaderTimeout = 5 * time.Second
	}
	if config.ReadTimeout == 0 {
		config.ReadTimeout = 15 * time.Second
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = 30 * time.Second
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = 60 * time.Second
	}
	if config.ShutdownTimeout == 0 {
		config.ShutdownTimeout = 10 * time.Second
	}
	if config.MaxHeaderBytes == 0 {
		config.MaxHeaderBytes = 1 << 20
	}
	server := &Server{
		config: config,
		httpServer: &http.Server{
			Addr:              config.Address,
			Handler:           handler.Routes(),
			ReadTimeout:       config.ReadTimeout,
			WriteTimeout:      config.WriteTimeout,
			IdleTimeout:       config.IdleTimeout,
			ReadHeaderTimeout: config.ReadHeaderTimeout,
			MaxHeaderBytes:    config.MaxHeaderBytes,
		},
	}
	lifecycle.Append(fx.Hook{
		OnStart: server.start,
		OnStop:  server.stop,
	})
	return server, nil
}

func (s *Server) start(context.Context) error {
	listener, err := net.Listen("tcp", s.config.Address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.config.Address, err)
	}
	s.listener = listener
	go func() {
		err := s.httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server stopped unexpectedly: %v", err)
		}
	}()
	return nil
}

func (s *Server) stop(ctx context.Context) error {
	if s.listener == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, s.config.ShutdownTimeout)
	defer cancel()
	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		_ = s.httpServer.Close()
		return fmt.Errorf("shutdown http server: %w", err)
	}
	return nil
}
