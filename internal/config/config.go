package config

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Address, DB, Issuer, JWKS, Audience, Region, Endpoint, QueueName, OutputName, DLQName string
	SigningKeys                                                                           map[string]string
	MaxAttempts                                                                           int
	ReferenceTTL                                                                          time.Duration
	Workers                                                                               int
}

func env(k, d string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return d
}
func Load() (Config, error) {
	c := Config{Address: env("HTTP_ADDR", ":8080"), DB: os.Getenv("DATABASE_URL"), Issuer: os.Getenv("OIDC_ISSUER"), JWKS: os.Getenv("OIDC_JWKS_URL"), Audience: env("OIDC_AUDIENCE", "wagering-api"), Region: env("AWS_REGION", "us-east-1"), Endpoint: os.Getenv("SQS_ENDPOINT"), QueueName: env("SQS_QUEUE", "wager-transactions.fifo"), OutputName: env("SQS_OUTPUT_QUEUE", "wager-events.fifo"), DLQName: env("SQS_DLQ", "wager-transactions-dlq.fifo")}
	var err error
	c.MaxAttempts, err = strconv.Atoi(env("REFERENCE_MAX_ATTEMPTS", "12"))
	if err != nil || c.MaxAttempts < 1 {
		return c, errors.New("invalid REFERENCE_MAX_ATTEMPTS")
	}
	c.ReferenceTTL, err = time.ParseDuration(env("REFERENCE_TTL", "24h"))
	if err != nil || c.ReferenceTTL <= 0 {
		return c, errors.New("invalid REFERENCE_TTL")
	}
	c.Workers, err = strconv.Atoi(env("SQS_WORKERS", "4"))
	if err != nil || c.Workers < 1 || c.Workers > 32 {
		return c, errors.New("invalid SQS_WORKERS")
	}
	if c.DB == "" || c.Issuer == "" || c.JWKS == "" {
		return c, errors.New("DATABASE_URL, OIDC_ISSUER and OIDC_JWKS_URL are required")
	}
	for _, s := range []string{c.Issuer, c.JWKS} {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return c, errors.New("invalid OIDC URL")
		}
	}
	if err = json.Unmarshal([]byte(os.Getenv("PROVIDER_SIGNING_KEYS")), &c.SigningKeys); err != nil || len(c.SigningKeys) == 0 {
		return c, errors.New("PROVIDER_SIGNING_KEYS must be a nonempty JSON map")
	}
	for p, k := range c.SigningKeys {
		if p == "" || len(k) < 32 {
			return c, errors.New("provider signing keys must have at least 32 bytes")
		}
	}
	return c, nil
}
