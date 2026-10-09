package ports

import (
	"context"
	"errors"
)

var ErrUnauthenticated = errors.New("request is unauthenticated")

type Principal struct {
	Subject    string
	ClientID   string
	ProviderID string
	Roles      []string
}

func (p Principal) HasRole(role string) bool {
	for _, candidate := range p.Roles {
		if candidate == role {
			return true
		}
	}
	return false
}

type Authenticator interface {
	Authenticate(context.Context, string) (Principal, error)
}
