package health

import (
	"context"
	"errors"

	"github.com/junglegaming/backend-challenge-go/internal/application/ports"
)

var ErrInvalidDependencies = errors.New("at least one health checker is required")

type DependencyStatus struct {
	Name string
	OK   bool
}

type Readiness struct {
	checkers []ports.HealthChecker
}

func NewReadiness(checkers ...ports.HealthChecker) (*Readiness, error) {
	if len(checkers) == 0 {
		return nil, ErrInvalidDependencies
	}
	for _, checker := range checkers {
		if checker == nil || checker.Name() == "" {
			return nil, ErrInvalidDependencies
		}
	}
	return &Readiness{checkers: append([]ports.HealthChecker(nil), checkers...)}, nil
}

func (r *Readiness) Check(ctx context.Context) ([]DependencyStatus, bool) {
	statuses := make([]DependencyStatus, 0, len(r.checkers))
	ready := true
	for _, checker := range r.checkers {
		ok := checker.Check(ctx) == nil
		statuses = append(statuses, DependencyStatus{Name: checker.Name(), OK: ok})
		ready = ready && ok
	}
	return statuses, ready
}
