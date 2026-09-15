// Package rediskv implements schemas.KVStore against Redis, so the KV-backed
// features Bifrost already ships (session stickiness, the routing plugin's warm
// coordinator, the job sweeper's poll lease) work across nodes instead of per
// process.
//
// This is MAAS_TECH_DESIGN.md Phase 1 item 4. Nothing upstream is modified: the
// store is injected via schemas.BifrostConfig.KVStore, which the core treats as
// an interface it does not own (core/schemas/bifrost.go:38).
//
// The config mirrors framework/vectorstore/redis.go's RedisConfig rather than
// inventing its own shape — same SecretVar fields, same TLS handling, same pool
// knobs — so operators configure one Redis the same way twice, and so the
// production-grade bits (RESP3, cluster mode, CA pinning) are inherited rather
// than reimplemented.
package rediskv

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/redis/go-redis/v9"
)

// Config configures the Redis-backed KV store.
type Config struct {
	// Connection
	Addr     *schemas.SecretVar `json:"addr"`
	Username *schemas.SecretVar `json:"username,omitempty"`
	Password *schemas.SecretVar `json:"password,omitempty"`
	DB       *schemas.SecretVar `json:"db,omitempty"`

	// TLS
	UseTLS             *schemas.SecretVar `json:"use_tls,omitempty"`
	InsecureSkipVerify *schemas.SecretVar `json:"insecure_skip_verify,omitempty"`
	CACertPEM          *schemas.SecretVar `json:"ca_cert_pem,omitempty"`

	// Cluster
	ClusterMode *schemas.SecretVar `json:"cluster_mode,omitempty"`

	// Pool and timeouts
	PoolSize        int              `json:"pool_size,omitempty"`
	MaxActiveConns  int              `json:"max_active_conns,omitempty"`
	MinIdleConns    int              `json:"min_idle_conns,omitempty"`
	MaxIdleConns    int              `json:"max_idle_conns,omitempty"`
	ConnMaxLifetime schemas.Duration `json:"conn_max_lifetime,omitempty"`
	ConnMaxIdleTime schemas.Duration `json:"conn_max_idle_time,omitempty"`
	DialTimeout     schemas.Duration `json:"dial_timeout,omitempty"`
	ReadTimeout     schemas.Duration `json:"read_timeout,omitempty"`
	WriteTimeout    schemas.Duration `json:"write_timeout,omitempty"`

	// OpTimeout bounds each individual KV operation.
	//
	// This one has no counterpart in the vectorstore config, and it is not
	// optional in practice: schemas.KVStore's methods take no context, so there
	// is no caller deadline to inherit and no cancellation to propagate. Every
	// call sits on a request-serving path (key selection, warm claim), so an
	// unbounded wait on an unhealthy Redis would translate directly into
	// request latency. Defaults to defaultOpTimeout.
	OpTimeout schemas.Duration `json:"op_timeout,omitempty"`

	// KeyPrefix namespaces every key this store touches.
	//
	// Bifrost's own keys are already collision-safe (each consumer prefixes and
	// hashes its own), so this exists for deployment hygiene: one Redis shared
	// by several environments, or by the gateway and something else, stays
	// separable and independently flushable.
	KeyPrefix string `json:"key_prefix,omitempty"`
}

const (
	defaultOpTimeout = 2 * time.Second

	// DefaultKeyPrefix keeps gateway keys distinguishable in a shared Redis.
	DefaultKeyPrefix = "bifrost:kv:"
)

// Validate checks the fields the constructor cannot default.
func Validate(c *Config) error {
	if c == nil {
		return fmt.Errorf("rediskv: config is required")
	}
	if c.Addr == nil || c.Addr.GetValue() == "" {
		return fmt.Errorf("rediskv: addr is required")
	}
	if c.ClusterMode.CoerceBool(false) && c.DB.CoerceInt(0) != 0 {
		return fmt.Errorf("rediskv: redis cluster mode does not support database selection (db must be 0)")
	}
	return nil
}

// newClient builds the Redis client. Mirrors
// framework/vectorstore/redis.go's construction, including RESP3 and the
// cluster/standalone split.
func newClient(c *Config) (redis.UniversalClient, error) {
	var tlsConfig *tls.Config
	if c.UseTLS.CoerceBool(false) {
		tlsConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: c.InsecureSkipVerify.CoerceBool(false),
		}
		if c.CACertPEM != nil && c.CACertPEM.GetValue() != "" {
			rootCAs, err := systemCertPoolWithCA(c.CACertPEM.GetValue())
			if err != nil {
				return nil, fmt.Errorf("rediskv: failed to configure TLS CA certificate: %w", err)
			}
			tlsConfig.RootCAs = rootCAs
		}
	}

	username := ""
	if c.Username != nil {
		username = c.Username.GetValue()
	}
	password := ""
	if c.Password != nil {
		password = c.Password.GetValue()
	}

	if c.ClusterMode.CoerceBool(false) {
		return redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:           []string{c.Addr.GetValue()},
			Username:        username,
			Password:        password,
			Protocol:        3,
			TLSConfig:       tlsConfig,
			PoolSize:        c.PoolSize,
			MaxActiveConns:  c.MaxActiveConns,
			MinIdleConns:    c.MinIdleConns,
			MaxIdleConns:    c.MaxIdleConns,
			ConnMaxLifetime: time.Duration(c.ConnMaxLifetime),
			ConnMaxIdleTime: time.Duration(c.ConnMaxIdleTime),
			DialTimeout:     time.Duration(c.DialTimeout),
			ReadTimeout:     time.Duration(c.ReadTimeout),
			WriteTimeout:    time.Duration(c.WriteTimeout),
		}), nil
	}

	return redis.NewClient(&redis.Options{
		Addr:            c.Addr.GetValue(),
		Username:        username,
		Password:        password,
		DB:              c.DB.CoerceInt(0),
		Protocol:        3,
		TLSConfig:       tlsConfig,
		PoolSize:        c.PoolSize,
		MaxActiveConns:  c.MaxActiveConns,
		MinIdleConns:    c.MinIdleConns,
		MaxIdleConns:    c.MaxIdleConns,
		ConnMaxLifetime: time.Duration(c.ConnMaxLifetime),
		ConnMaxIdleTime: time.Duration(c.ConnMaxIdleTime),
		DialTimeout:     time.Duration(c.DialTimeout),
		ReadTimeout:     time.Duration(c.ReadTimeout),
		WriteTimeout:    time.Duration(c.WriteTimeout),
	}), nil
}

func systemCertPoolWithCA(caCertPEM string) (*x509.CertPool, error) {
	rootCAs, err := x509.SystemCertPool()
	if err != nil {
		rootCAs = x509.NewCertPool()
	}
	if !rootCAs.AppendCertsFromPEM([]byte(caCertPEM)) {
		return nil, fmt.Errorf("failed to parse CA certificate PEM")
	}
	return rootCAs, nil
}
