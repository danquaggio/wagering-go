package auth

import (
	"context"
	"errors"
	"example.com/wagering/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
	"net/http"
	"strings"
	"time"
)

type Identity struct {
	Provider string
	Internal bool
}
type Auth struct {
	verifier *oidc.IDTokenVerifier
	allowed  map[string]string
}

func New(c config.Config) *Auth {
	client := &http.Client{Timeout: 5 * time.Second}
	ctx := oidc.ClientContext(context.Background(), client)
	keys := oidc.NewRemoteKeySet(ctx, c.JWKS)
	return &Auth{oidc.NewVerifier(c.Issuer, keys, &oidc.Config{ClientID: c.Audience, SupportedSigningAlgs: []string{"RS256"}}), c.SigningKeys}
}
func (a *Auth) Verify(ctx context.Context, header string) (Identity, error) {
	parts := strings.Split(header, " ")
	if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" {
		return Identity{}, errors.New("missing bearer token")
	}
	token, err := a.verifier.Verify(ctx, parts[1])
	if err != nil {
		return Identity{}, errors.New("invalid access token")
	}
	var claims struct {
		AZP      string `json:"azp"`
		Provider string `json:"provider_id"`
		Role     string `json:"service_role"`
		Type     string `json:"typ"`
	}
	if err = token.Claims(&claims); err != nil || claims.Type != "Bearer" {
		return Identity{}, errors.New("invalid access token claims")
	}
	if claims.AZP == "wallet-service" && claims.Role == "wallet:manage" && claims.Provider == "" {
		return Identity{Internal: true}, nil
	}
	if _, ok := a.allowed[claims.Provider]; !ok || claims.Provider != claims.AZP || claims.Role != "wager:write" {
		return Identity{}, errors.New("unauthorized client")
	}
	return Identity{Provider: claims.Provider}, nil
}
