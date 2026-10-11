package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

func TestFxServerLifecycleStartsServesAndStops(t *testing.T) {
	var server *Server
	app := fx.New(
		fx.Provide(func() *Handler { return &Handler{} }),
		fx.Supply(ServerConfig{Address: "127.0.0.1:0"}),
		fx.Provide(NewServer),
		fx.Populate(&server),
		fx.WithLogger(func() fxevent.Logger { return fxevent.NopLogger }),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("build Fx application: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start Fx application: %v", err)
	}
	needsStop := true
	defer func() {
		if needsStop {
			_ = app.Stop(context.Background())
		}
	}()

	response, err := http.Get("http://" + server.listener.Addr().String() + "/health/live")
	if err != nil {
		t.Fatalf("request liveness endpoint: %v", err)
	}
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || body["status"] != "alive" {
		t.Fatalf("liveness response = (%d, %q), want (200, alive)", response.StatusCode, body["status"])
	}
	if err := app.Stop(ctx); err != nil {
		t.Fatalf("stop Fx application: %v", err)
	}
	needsStop = false
	if _, err := http.Get("http://" + server.listener.Addr().String() + "/health/live"); err == nil {
		t.Fatal("HTTP server accepted a request after Fx shutdown")
	}
}
