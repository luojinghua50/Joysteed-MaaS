// Package rlsspike answers the D11b open question: can Postgres RLS enforce
// tenant isolation underneath Bifrost's existing data-plane code, without
// patching the ~387 raw s.DB() call sites?
//
// The spike deliberately opens its pools through framework/postgresconn so it
// exercises the production connection path (ApplyPoolTuning, pgx driver,
// libpq DSN) rather than a hand-rolled gorm.Open that would not reproduce
// pool-reuse behaviour.
//
// Requires a throwaway Postgres. To bring one up:
//
//	docker run -d --name maas-rls-spike \
//	  -e POSTGRES_PASSWORD=spike_password -e POSTGRES_USER=spike \
//	  -e POSTGRES_DB=spike -p 55432:5432 postgres:16-alpine
//
// Then:
//
//	go test ./tests/rlsspike/ -count=1
//	go test ./tests/rlsspike/ -bench . -benchtime 1000x -run '^$' -count=3
//
// Teardown: docker rm -f maas-rls-spike
// Override the target with MAAS_SPIKE_PG_{HOST,PORT,USER,PASSWORD,DB}.
//
// Findings are written up in MAAS_TECH_DESIGN.md §9.2.
package rlsspike

import (
	"fmt"
	"os"

	"github.com/luojinghua50/Joysteed-MaaS/internal/pglock"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/postgresconn"
	"gorm.io/gorm"
)

// TenantIDSetting is the session variable the RLS policies read. Named to match
// what a real deployment would use; the leading namespace is required by
// Postgres for custom settings.
const TenantIDSetting = "app.tenant_id"

func secret(v string) *schemas.SecretVar {
	return &schemas.SecretVar{Val: v}
}

// There are three distinct roles here because RLS enforcement depends on which
// one the connection uses, and Postgres exempts two of the three:
//
//   - bootstrap (superuser): creates roles. Superusers AND roles with BYPASSRLS
//     ignore RLS entirely — FORCE ROW LEVEL SECURITY does not constrain them.
//   - owner (non-superuser, owns the table): exempt from its own table's RLS
//     unless FORCE ROW LEVEL SECURITY is set.
//   - app (non-superuser, non-owner): always constrained by RLS.
//
// This matters for Bifrost specifically: framework/configstore/postgres.go
// builds the migration pool and the runtime pool from the SAME config, so the
// runtime role is the table owner. A deployment that also uses a superuser (the
// common default for a container Postgres) gets no isolation at all.
func bootstrapConfig() *postgresconn.Config {
	return &postgresconn.Config{
		Host:     secret(env("MAAS_SPIKE_PG_HOST", "localhost")),
		Port:     secret(env("MAAS_SPIKE_PG_PORT", "55432")),
		User:     secret(env("MAAS_SPIKE_PG_USER", "spike")),
		Password: secret(env("MAAS_SPIKE_PG_PASSWORD", "spike_password")),
		DBName:   secret(env("MAAS_SPIKE_PG_DB", "spike")),
		SSLMode:  secret("disable"),
		// Deliberately small and reused: a 2-connection pool makes cross-request
		// connection reuse observable, which is the whole point of the
		// session-scoped leak test.
		MaxIdleConns: 2,
		MaxOpenConns: 2,
	}
}

func withUser(user, password string) *postgresconn.Config {
	c := bootstrapConfig()
	c.User = secret(user)
	c.Password = secret(password)
	return c
}

// ownerConfig is a non-superuser role that owns the table — the shape Bifrost's
// runtime pool has today.
func ownerConfig() *postgresconn.Config {
	return withUser("maas_owner", "maas_owner_password")
}

// appConfig is a non-superuser, non-owner role.
func appConfig() *postgresconn.Config {
	return withUser("maas_app", "maas_app_password")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Open opens a pool through the production connection path.
func Open(c *postgresconn.Config) (*gorm.DB, error) {
	if err := postgresconn.Validate(c, true); err != nil {
		return nil, err
	}
	db, err := postgresconn.Open(postgresconn.BuildDSN(c), c, nil)
	if err != nil {
		return nil, err
	}
	if err := postgresconn.ApplyPoolTuning(db, c); err != nil {
		return nil, err
	}
	return db, nil
}

// OpenBootstrap opens the superuser pool (role creation, and the
// superuser-bypass test).
func OpenBootstrap() (*gorm.DB, error) { return Open(bootstrapConfig()) }

// OpenOwner opens the non-superuser table-owner pool.
func OpenOwner() (*gorm.DB, error) { return Open(ownerConfig()) }

// OpenApp opens the non-owner app-role pool.
func OpenApp() (*gorm.DB, error) { return Open(appConfig()) }

// ProviderKey mirrors the shape of the one table where isolation is hardest:
// config_keys carries both tenant-owned BYOK rows (TenantID non-null) and
// platform-pool rows (TenantID null) that every tenant may legitimately see.
// See MAAS_TECH_DESIGN.md §3.5.1.
type ProviderKey struct {
	ID       uint    `gorm:"primaryKey"`
	TenantID *string `gorm:"column:tenant_id;index"`
	Provider string  `gorm:"column:provider"`
	Label    string  `gorm:"column:label"`
}

func (ProviderKey) TableName() string { return "spike_provider_keys" }

// Setup creates the app role, the table, the RLS policy, and seed rows.
// force controls whether FORCE ROW LEVEL SECURITY is applied, which is the
// difference between "RLS constrains the owner too" and "the owner silently
// bypasses it".
func Setup(owner *gorm.DB, force bool) error {
	stmts := []string{
		`DROP TABLE IF EXISTS spike_provider_keys`,
		`CREATE TABLE spike_provider_keys (
			id BIGSERIAL PRIMARY KEY,
			tenant_id TEXT,
			provider TEXT NOT NULL,
			label TEXT NOT NULL
		)`,
		`CREATE INDEX ON spike_provider_keys (tenant_id)`,
		`ALTER TABLE spike_provider_keys ENABLE ROW LEVEL SECURITY`,
		// The policy encodes §3.5.1's rule: a tenant sees its own rows plus the
		// platform pool, and nothing else. current_setting(..., true) returns
		// NULL rather than erroring when the variable is unset.
		`CREATE POLICY tenant_isolation ON spike_provider_keys
			FOR ALL
			USING (
				tenant_id IS NULL
				OR tenant_id = current_setting('` + TenantIDSetting + `', true)
			)`,
	}
	if force {
		stmts = append(stmts, `ALTER TABLE spike_provider_keys FORCE ROW LEVEL SECURITY`)
	}
	for _, s := range stmts {
		if err := owner.Exec(s).Error; err != nil {
			return fmt.Errorf("setup %q: %w", s, err)
		}
	}
	return Seed(owner)
}

// Seed inserts one platform-pool row and two tenant-owned rows.
func Seed(owner *gorm.DB) error {
	a, b := "tenant-a", "tenant-b"
	rows := []ProviderKey{
		{TenantID: nil, Provider: "openai", Label: "platform-pool-key"},
		{TenantID: &a, Provider: "openai", Label: "tenant-a-byok"},
		{TenantID: &b, Provider: "openai", Label: "tenant-b-byok"},
	}
	// Inserts run as owner before FORCE is exercised by the read tests; when
	// FORCE is on, the policy's USING clause also gates INSERT via FOR ALL, so
	// seed with RLS temporarily off to keep the fixture independent of policy.
	if err := owner.Exec(`ALTER TABLE spike_provider_keys DISABLE ROW LEVEL SECURITY`).Error; err != nil {
		return err
	}
	if err := owner.Create(&rows).Error; err != nil {
		return err
	}
	return owner.Exec(`ALTER TABLE spike_provider_keys ENABLE ROW LEVEL SECURITY`).Error
}

// EnsureRoles creates the owner and app roles. Must run as the bootstrap
// superuser: CREATE ROLE needs superuser or CREATEROLE.
//
// Neither role gets BYPASSRLS. That is not a detail — a role with BYPASSRLS
// ignores every policy, so granting it (or reusing a superuser) silently
// disables tenant isolation with no error anywhere.
func EnsureRoles(bootstrap *gorm.DB) error {
	stmts := []string{
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'maas_owner') THEN
				CREATE ROLE maas_owner LOGIN PASSWORD 'maas_owner_password';
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'maas_app') THEN
				CREATE ROLE maas_app LOGIN PASSWORD 'maas_app_password';
			END IF;
		END $$`,
		// Drop as bootstrap, not as owner: a table left behind by an earlier run
		// may be owned by a different role, and DROP requires ownership. Once
		// maas_owner has created it, Setup's own drop suffices.
		`DROP TABLE IF EXISTS spike_provider_keys`,
	}
	for _, s := range stmts {
		if err := bootstrap.Exec(s).Error; err != nil {
			return fmt.Errorf("ensure roles %q: %w", s, err)
		}
	}
	// Under the shared-catalog lock: these GRANTs update the single pg_namespace
	// row for `public`, which other test packages' bootstraps also update.
	// Postgres does not do that under MVCC, so concurrent packages abort with
	// "tuple concurrently updated" (see internal/pglock). Postgres 15+ also
	// revoked the public schema's default CREATE grant, so maas_owner needs this
	// to create its own table.
	if err := pglock.WithSharedCatalog(bootstrap, func(tx *gorm.DB) error {
		for _, s := range []string{
			`GRANT USAGE, CREATE ON SCHEMA public TO maas_owner`,
			`GRANT USAGE ON SCHEMA public TO maas_app`,
		} {
			if err := tx.Exec(s).Error; err != nil {
				return fmt.Errorf("ensure roles %q: %w", s, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// GrantApp gives the app role DML on the table. Must run as the table owner.
func GrantApp(owner *gorm.DB) error {
	stmts := []string{
		`GRANT SELECT, INSERT, UPDATE, DELETE ON spike_provider_keys TO maas_app`,
		`GRANT USAGE, SELECT ON SEQUENCE spike_provider_keys_id_seq TO maas_app`,
	}
	for _, s := range stmts {
		if err := owner.Exec(s).Error; err != nil {
			return fmt.Errorf("grant app %q: %w", s, err)
		}
	}
	return nil
}
