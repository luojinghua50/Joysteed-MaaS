package tenant_test

import (
	"context"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/framework/queryscope"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// tenantKey mirrors the shape configstore's TableKey has AFTER the M1 migration
// adds the nullable tenant_id column. It is deliberately minimal: this suite
// tests the WHERE predicate, not TableKey's encryption hooks.
type tenantKey struct {
	ID       uint    `gorm:"primaryKey;autoIncrement"`
	Name     string  `gorm:"type:varchar(255)"`
	Provider string  `gorm:"type:varchar(50)"`
	TenantID *string `gorm:"type:varchar(36);index"`
}

func (tenantKey) TableName() string { return "config_keys" }

const (
	tenantA = "tenant-aaaa"
	tenantB = "tenant-bbbb"
)

func newKeyDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&tenantKey{}))

	ptr := func(s string) *string { return &s }
	seed := []tenantKey{
		{Name: "a-openai", Provider: "openai", TenantID: ptr(tenantA)},
		{Name: "a-anthropic", Provider: "anthropic", TenantID: ptr(tenantA)},
		{Name: "b-openai", Provider: "openai", TenantID: ptr(tenantB)},
		{Name: "b-bedrock", Provider: "bedrock", TenantID: ptr(tenantB)},
		{Name: "pool-openai", Provider: "openai", TenantID: nil},
		{Name: "pool-vertex", Provider: "vertex", TenantID: nil},
	}
	require.NoError(t, db.Create(&seed).Error)
	return db
}

// namesUnderScope applies a QueryScope the way an inner store's ScopedDB does,
// then returns the key names the caller can actually see.
func namesUnderScope(t *testing.T, db *gorm.DB, scope queryscope.QueryScope) []string {
	t.Helper()
	q := db.Model(&tenantKey{})
	if scope != nil {
		q = scope(q)
	}
	var out []string
	require.NoError(t, q.Order("name").Pluck("name", &out).Error)
	return out
}

// TestKeyScope_ThreeParty is the R9 verification named in MAAS_TECH_DESIGN.md
// §3.5.1: tenant A's result must equal {A's keys} ∪ {platform pool}, exactly,
// with nothing of tenant B's in it.
func TestKeyScope_ThreeParty(t *testing.T) {
	db := newKeyDB(t)

	scopeA, err := tenant.KeyScopeFor(tenantA)
	require.NoError(t, err)
	got := namesUnderScope(t, db, scopeA)

	assert.Equal(t,
		[]string{"a-anthropic", "a-openai", "pool-openai", "pool-vertex"},
		got,
		"tenant A must see exactly its own keys plus the platform pool")

	// Stated separately from the equality above: if the predicate is ever
	// loosened, this is the assertion that names the actual damage.
	assert.NotContains(t, got, "b-openai", "tenant B's BYOK key leaked to tenant A")
	assert.NotContains(t, got, "b-bedrock", "tenant B's BYOK key leaked to tenant A")
}

// The mirror case. A predicate that hardcodes one tenant passes the A-side test
// and fails here.
func TestKeyScope_ThreeParty_OtherDirection(t *testing.T) {
	db := newKeyDB(t)

	scopeB, err := tenant.KeyScopeFor(tenantB)
	require.NoError(t, err)
	got := namesUnderScope(t, db, scopeB)

	assert.Equal(t,
		[]string{"b-bedrock", "b-openai", "pool-openai", "pool-vertex"},
		got)
	assert.NotContains(t, got, "a-openai", "tenant A's BYOK key leaked to tenant B")
	assert.NotContains(t, got, "a-anthropic", "tenant A's BYOK key leaked to tenant B")
}

// The platform pool must stay reachable. This is the loud failure mode: writing
// `tenant_id = ?` instead of `tenant_id = ? OR tenant_id IS NULL` breaks BYOK
// tenants' access to shared capacity, and this test is what catches it.
func TestKeyScope_PlatformPoolReachable(t *testing.T) {
	db := newKeyDB(t)

	scopeA, err := tenant.KeyScopeFor(tenantA)
	require.NoError(t, err)
	got := namesUnderScope(t, db, scopeA)

	assert.Contains(t, got, "pool-openai", "platform pool must remain reachable by tenants")
	assert.Contains(t, got, "pool-vertex", "platform pool must remain reachable by tenants")
}

// A tenant with no BYOK keys at all sees the platform pool and nothing else.
// This is the D1 "platform-owned only" tenant, which must not accidentally
// inherit another tenant's keys.
func TestKeyScope_TenantWithoutBYOK(t *testing.T) {
	db := newKeyDB(t)

	scope, err := tenant.KeyScopeFor("tenant-cccc-no-byok")
	require.NoError(t, err)

	assert.Equal(t, []string{"pool-openai", "pool-vertex"}, namesUnderScope(t, db, scope))
}

// Fail-closed: an empty tenant must produce an error, never a usable scope.
// Returning (nil, nil) here would be indistinguishable from "no restriction"
// downstream, which is the R1 fail-open trap.
func TestKeyScope_EmptyTenantFailsClosed(t *testing.T) {
	scope, err := tenant.KeyScopeFor("")

	require.Error(t, err)
	assert.ErrorIs(t, err, tenant.ErrMissingTenant)
	assert.Nil(t, scope, "no scope may be returned when the tenant is unresolved")
}

// StrictScopeFor has no platform tier: NULL tenant_id rows belong to nobody.
func TestStrictScope_ExcludesNullAndOtherTenants(t *testing.T) {
	db := newKeyDB(t)

	scope, err := tenant.StrictScopeFor(tenant.KeyColumn, tenantA)
	require.NoError(t, err)
	got := namesUnderScope(t, db, scope)

	assert.Equal(t, []string{"a-anthropic", "a-openai"}, got)
	assert.NotContains(t, got, "pool-openai", "strict scope must not expose platform rows")
	assert.NotContains(t, got, "b-openai")
}

func TestStrictScope_FailsClosed(t *testing.T) {
	scope, err := tenant.StrictScopeFor(tenant.KeyColumn, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, tenant.ErrMissingTenant)
	assert.Nil(t, scope)

	scope, err = tenant.StrictScopeFor("", tenantA)
	require.Error(t, err)
	assert.Nil(t, scope)
}

// The scope must survive the ctx round-trip that inner stores actually use:
// WithQueryScope on the way in, FromContext + ScopedDB on the way out.
func TestKeyScope_SurvivesContextRoundTrip(t *testing.T) {
	db := newKeyDB(t)

	scopeA, err := tenant.KeyScopeFor(tenantA)
	require.NoError(t, err)
	ctx := queryscope.WithQueryScope(context.Background(), scopeA)

	recovered := queryscope.FromContext(ctx)
	require.NotNil(t, recovered, "scope must be retrievable the way ScopedDB retrieves it")

	assert.Equal(t,
		[]string{"a-anthropic", "a-openai", "pool-openai", "pool-vertex"},
		namesUnderScope(t, db, recovered))
}

// Documents the upstream default this package exists to contain: a context with
// no scope yields nil, and nil means every row. Not a bug in queryscope — the
// correct OSS single-tenant default — but the reason TenantScopedStore must
// refuse to run tenant-level queries when FromContext returns nil.
func TestNoScopeOnContext_IsFailOpen(t *testing.T) {
	db := newKeyDB(t)

	assert.Nil(t, queryscope.FromContext(context.Background()))

	all := namesUnderScope(t, db, queryscope.FromContext(context.Background()))
	assert.Len(t, all, 6, "no scope returns every row across every tenant")
	assert.Contains(t, all, "b-openai")
}

// PlatformScope is an explicit, greppable opt-out rather than a nil scope.
func TestPlatformScope_SeesEverythingButIsExplicit(t *testing.T) {
	db := newKeyDB(t)

	scope := tenant.PlatformScope()
	require.NotNil(t, scope, "platform reads must carry a real scope, not nil")
	assert.Len(t, namesUnderScope(t, db, scope), 6)
}
