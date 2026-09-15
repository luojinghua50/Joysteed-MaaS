package migrate_test

import (
	"strings"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/migrate"
	"github.com/stretchr/testify/require"
)

func TestCompositeIndexes_AreTenantLeadingAndNamedSafely(t *testing.T) {
	require.NotEmpty(t, migrate.CompositeIndexes)
	seenNames := map[string]struct{}{}
	seenTables := map[string]bool{}
	for _, index := range migrate.CompositeIndexes {
		require.NotEmpty(t, index.Name)
		require.NotEmpty(t, index.Table)
		require.NotEmpty(t, index.Column)
		require.LessOrEqual(t, len(index.Name), 63)
		require.NotContains(t, index.Name, " ")
		_, duplicate := seenNames[index.Name]
		require.False(t, duplicate, "duplicate index %s", index.Name)
		seenNames[index.Name] = struct{}{}
		seenTables[index.Table] = true
	}
	for _, table := range append(append([]string{}, migrate.BClassTables...), migrate.CClassTable, migrate.SessionsTable) {
		require.True(t, seenTables[table], "tenant table %s needs a tenant-leading lookup index", table)
	}
}

func TestIndexMigrations_CoverEveryDeclaredIndexExactlyOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range migrate.IndexMigrations() {
		require.True(t, strings.HasPrefix(m.ID, "20260915-01-index-"))
		require.False(t, seen[m.ID], "duplicate migration %s", m.ID)
		require.NotNil(t, m.Migrate)
		require.NotNil(t, m.Rollback)
		seen[m.ID] = true
	}
	require.Len(t, seen, len(migrate.CompositeIndexes))
}
