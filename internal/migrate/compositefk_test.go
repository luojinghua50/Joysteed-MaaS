package migrate_test

import (
	"fmt"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/migrate"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These run against Postgres as a non-superuser table owner, reusing
// runner_test.go's newMigratorDB. The role matters for the same reason it does
// there: several assertions below depend on the RLS policies actually filtering,
// and a superuser bypasses them unconditionally (R31).

// edge looks up a declared edge by name, so tests exercise the real TenantFKs
// entry rather than a literal built in the test. A renamed edge then fails here
// with the name it looked for, instead of passing against a fixture that no
// longer resembles anything shipped.
func edge(t *testing.T, name string) migrate.TenantFK {
	t.Helper()
	for _, fk := range migrate.TenantFKs {
		if fk.Name == name {
			return fk
		}
	}
	t.Fatalf("no declared edge named %q", name)
	return migrate.TenantFK{}
}

// fkTables creates one edge's parent and child.
//
// Column TYPES are the test's own (BIGSERIAL/BIGINT throughout) rather than
// upstream's mix of varchar and bigint ids. What is under test is the
// constraint's semantics, which do not depend on the id type; keeping one type
// lets every edge share this fixture. That the COLUMN NAMES match upstream is
// checked separately, against the real structs, by
// TestTenantFKs_MatchUpstreamSchema.
func fkTables(t *testing.T, db *gorm.DB, fk migrate.TenantFK) {
	t.Helper()
	null := ""
	if !fk.Nullable {
		null = " NOT NULL"
	}
	for _, s := range []string{
		fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, fk.Child),
		fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, fk.Parent),
		fmt.Sprintf(`CREATE TABLE %s (id BIGSERIAL PRIMARY KEY, tenant_id TEXT, label TEXT)`, fk.Parent),
		fmt.Sprintf(`CREATE TABLE %s (id BIGSERIAL PRIMARY KEY, tenant_id TEXT, label TEXT, %s BIGINT%s)`,
			fk.Child, fk.ChildColumn, null),
	} {
		require.NoError(t, db.Exec(s).Error, s)
	}
	t.Cleanup(func() {
		_ = db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, fk.Child)).Error
		_ = db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, fk.Parent)).Error
	})
}

// insertParent adds a parent row for a tenant and returns its id.
func insertParent(t *testing.T, db *gorm.DB, fk migrate.TenantFK, tenantID string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, db.Raw(fmt.Sprintf(
		`INSERT INTO %s (tenant_id, label) VALUES (?, 'p') RETURNING id`, fk.Parent),
		tenantID).Scan(&id).Error)
	return id
}

// insertChild attempts a child row and returns the error rather than asserting,
// since half these tests want the failure.
func insertChild(db *gorm.DB, fk migrate.TenantFK, tenantID *string, parentID *int64) error {
	return db.Exec(fmt.Sprintf(
		`INSERT INTO %s (tenant_id, label, %s) VALUES (?, 'c', ?)`, fk.Child, fk.ChildColumn),
		tenantID, parentID).Error
}

// newSchemaDB opens a handle whose search_path is a schema of this test's own,
// creating it fresh and dropping it afterwards.
//
// Needed because the only faithful way to check our column names is to create the
// REAL upstream tables, and those names collide with the fixtures in
// runner_test.go and in internal/tenant's keyscope_test.go — different packages,
// so `go test ./...` runs them concurrently.
//
// Connects as the superuser, unlike every other fixture here. That is safe for
// this one test and only this one: it asserts on column metadata, never on row
// visibility, so the superuser's RLS exemption (R31) cannot make an isolation
// claim pass vacuously. There is nothing to isolate.
func newSchemaDB(t *testing.T, schema string) *gorm.DB {
	t.Helper()
	super := open(t, env("MAAS_SPIKE_PG_USER", "spike"),
		env("MAAS_SPIKE_PG_PASSWORD", "spike_password"))
	require.NoError(t, super.Exec(`DROP SCHEMA IF EXISTS `+schema+` CASCADE`).Error)
	require.NoError(t, super.Exec(`CREATE SCHEMA `+schema).Error)
	t.Cleanup(func() { _ = super.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`).Error })

	db, err := gorm.Open(postgres.Open(
		dsn(env("MAAS_SPIKE_PG_USER", "spike"), env("MAAS_SPIKE_PG_PASSWORD", "spike_password"))+
			" search_path="+schema), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	var current string
	require.NoError(t, db.Raw(`SELECT current_schema()`).Scan(&current).Error)
	require.Equal(t, schema, current, "fixtures must not land in a schema other packages use")
	return db
}

// notNullAndKeyed brings an edge to the state pass 3 requires: both tables'
// tenant_id NOT NULL, and the parent carrying its unique key.
func notNullAndKeyed(t *testing.T, db *gorm.DB, fk migrate.TenantFK) {
	t.Helper()
	require.NoError(t, migrate.SetTenantColumnNotNull(db, fk.Parent))
	require.NoError(t, migrate.SetTenantColumnNotNull(db, fk.Child))
	require.NoError(t, migrate.AddParentTenantKey(db, fk.Parent))
}

// TestAddTenantFK_RefusesACrossTenantChild is the constraint's whole purpose.
//
// The row this rejects is one both per-table policies accept: the child's own
// tenant_id is 't-a' and the parent's is 't-b', so each row is individually
// well-formed and passes its own table's predicate. A policy is evaluated per row
// per table and cannot see the pair, so nothing but this constraint refuses it.
func TestAddTenantFK_RefusesACrossTenantChild(t *testing.T) {
	db := newMigratorDB(t)
	fk := edge(t, "fk_pv_tenant_prompt")
	fkTables(t, db, fk)

	parentOfB := insertParent(t, db, fk, "t-b")
	// Attribute the child rows before tightening, since NOT NULL comes first.
	require.NoError(t, insertChild(db, fk, ptr("t-b"), &parentOfB))
	notNullAndKeyed(t, db, fk)
	require.NoError(t, migrate.AddTenantFK(db, fk))

	err := insertChild(db, fk, ptr("t-a"), &parentOfB)
	require.Error(t, err, "tenant t-a must not reference tenant t-b's parent row")
	require.Contains(t, err.Error(), fk.Name)
}

// TestAddTenantFK_AllowsASameTenantChild guards the other direction. A constraint
// that rejected everything would satisfy the test above and break the product.
func TestAddTenantFK_AllowsASameTenantChild(t *testing.T) {
	db := newMigratorDB(t)
	fk := edge(t, "fk_pv_tenant_prompt")
	fkTables(t, db, fk)

	parentOfA := insertParent(t, db, fk, "t-a")
	require.NoError(t, insertChild(db, fk, ptr("t-a"), &parentOfA))
	notNullAndKeyed(t, db, fk)
	require.NoError(t, migrate.AddTenantFK(db, fk))

	require.NoError(t, insertChild(db, fk, ptr("t-a"), &parentOfA))
}

// TestAddTenantFK_AllowsANullPointerOnANullableEdge covers the case that rules
// MATCH FULL out.
//
// prompts.folder_id is nullable: a prompt in no folder is normal, not an error.
// Under MATCH FULL a row with tenant_id set and folder_id NULL is a partial match
// and is REJECTED — so MATCH FULL would make every unfoldered prompt
// unwritable. MATCH SIMPLE skips the check when any referencing column is NULL,
// which is why the FKs here use it.
func TestAddTenantFK_AllowsANullPointerOnANullableEdge(t *testing.T) {
	db := newMigratorDB(t)
	fk := edge(t, "fk_prompts_tenant_folder")
	require.True(t, fk.Nullable, "this test is about the nullable case")
	fkTables(t, db, fk)

	notNullAndKeyed(t, db, fk)
	require.NoError(t, migrate.AddTenantFK(db, fk))

	require.NoError(t, insertChild(db, fk, ptr("t-a"), nil),
		"a NULL pointer must stay writable; MATCH FULL would reject this row")
}

// TestAddTenantFK_RefusesWhileTheTenantColumnIsNullable pins the ordering
// correction, and pins it as a COUNTERFACTUAL rather than as an assertion about
// our own error.
//
// §3.4 and §7.4.6 both order the steps "backfill → composite FK → SET NOT NULL".
// The first half of this test shows AddTenantFK refusing that order. The second
// half installs the constraint by hand in exactly that order and demonstrates what
// it does: the ALTER TABLE succeeds, the constraint appears in the catalog, and a
// cross-tenant child with a NULL tenant_id is inserted WITHOUT COMPLAINT, because
// MATCH SIMPLE skips any row with a NULL in the referencing columns.
//
// Without the second half this would only prove "our function returns an error",
// not "the order it refuses is genuinely unsafe" — and an unenforced constraint
// that is present in the schema is worse than a missing one, because it reads as
// covered.
func TestAddTenantFK_RefusesWhileTheTenantColumnIsNullable(t *testing.T) {
	db := newMigratorDB(t)
	fk := edge(t, "fk_pv_tenant_prompt")
	fkTables(t, db, fk)
	require.NoError(t, migrate.SetTenantColumnNotNull(db, fk.Parent))
	require.NoError(t, migrate.AddParentTenantKey(db, fk.Parent))
	parentOfB := insertParent(t, db, fk, "t-b")

	err := migrate.AddTenantFK(db, fk)
	require.ErrorIs(t, err, migrate.ErrTenantColumnNullable)
	require.Contains(t, err.Error(), fk.Child)

	// The documented order, by hand. Same DDL AddTenantFK would emit.
	require.NoError(t, db.Exec(fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (tenant_id, %s)
		 REFERENCES %s (tenant_id, id) MATCH SIMPLE`,
		fk.Child, fk.Name, fk.ChildColumn, fk.Parent)).Error,
		"the unsafe order still produces a valid constraint — that is the problem")

	require.NoError(t, insertChild(db, fk, nil, &parentOfB),
		"MATCH SIMPLE skipped the check because tenant_id is NULL: "+
			"the constraint exists and does not cover the unattributed rows")

	// And once the NULL is removed, the same shape refuses it.
	require.NoError(t, db.Exec(fmt.Sprintf(`DELETE FROM %s`, fk.Child)).Error)
	require.NoError(t, migrate.SetTenantColumnNotNull(db, fk.Child))
	require.Error(t, insertChild(db, fk, ptr("t-a"), &parentOfB),
		"with tenant_id NOT NULL the identical constraint does refuse the pair")
}

// TestAddTenantFK_LeavesUpstreamCascadeWorking pins the ON DELETE decision.
//
// Our constraint is NO ACTION while upstream's single-column FK on the same edge
// is ON DELETE CASCADE. Two constraints on overlapping columns with different
// referential actions is exactly the situation where a plausible-sounding argument
// ("NO ACTION will block the parent delete the cascade is trying to perform") is
// worth checking rather than reasoning about: if it were right, deleting any prompt
// in production would fail once these constraints ship.
//
// It is not right, because NO ACTION defers to end of statement, by which point the
// cascade has already removed the referencing row. This test is here so that claim
// is measured rather than asserted in a comment.
func TestAddTenantFK_LeavesUpstreamCascadeWorking(t *testing.T) {
	db := newMigratorDB(t)
	fk := edge(t, "fk_pv_tenant_prompt")
	fkTables(t, db, fk)

	// Upstream's own FK, in the shape tables/promptVersions.go declares it.
	require.NoError(t, db.Exec(fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT fk_upstream_shape FOREIGN KEY (%s)
		 REFERENCES %s (id) ON DELETE CASCADE`, fk.Child, fk.ChildColumn, fk.Parent)).Error)

	parent := insertParent(t, db, fk, "t-a")
	require.NoError(t, insertChild(db, fk, ptr("t-a"), &parent))
	notNullAndKeyed(t, db, fk)
	require.NoError(t, migrate.AddTenantFK(db, fk))

	require.NoError(t, db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, fk.Parent), parent).Error,
		"our NO ACTION constraint must not block upstream's cascade")

	var remaining int64
	require.NoError(t, db.Raw(fmt.Sprintf(`SELECT count(*) FROM %s`, fk.Child)).Scan(&remaining).Error)
	require.Zero(t, remaining, "the cascade should have removed the child row")
}

// TestAddTenantFK_RefusesANonTenantScopedParent pins the guard that catches the
// D-class gap (§7.6.2).
//
// governance_budgets.model_config_id points at governance_model_configs, which the
// audit says needs a tenant_id but which is not in BClassTables and so has none.
// Postgres's own error for that case names the missing column without saying which
// of the two tables lacks it, on a statement mentioning both.
func TestAddTenantFK_RefusesANonTenantScopedParent(t *testing.T) {
	db := newMigratorDB(t)
	err := migrate.AddTenantFK(db, migrate.TenantFK{
		Name:        "fk_budget_tenant_modelconfig",
		Child:       "governance_budgets",
		ChildColumn: "model_config_id",
		Parent:      "governance_model_configs",
		Nullable:    true,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not tenant-scoped")
	require.Contains(t, err.Error(), "governance_model_configs")
}

// upstreamStructs is one Go struct per table named in TenantFKs, parent and child.
//
// Written out rather than discovered, because the mapping from a table name to the
// struct that declares it is exactly what could be got wrong: TableVirtualKeyVirtualMCP
// declares enterprise_mcp_tool_group_virtual_keys, and its VirtualMCPID field maps to
// a column called tool_group_id. Nothing about either name follows from the other.
func upstreamStructs() []any {
	return []any{
		&tables.TableCustomer{}, &tables.TableTeam{}, &tables.TableVirtualKey{},
		&tables.TableVirtualKeyProviderConfig{}, &tables.TableVirtualKeyMCPConfig{},
		&tables.TableVirtualKeyProviderConfigKey{}, &tables.TableBudget{},
		&tables.TableRateLimit{}, &tables.TablePrompt{}, &tables.TablePromptVersion{},
		&tables.TablePromptSession{}, &tables.TablePromptSessionMessage{},
		&tables.TableFolder{}, &tables.TableSkill{}, &tables.TableSkillVersion{},
		&tables.TableSkillFile{}, &tables.TableSkillFileBlob{}, &tables.TableVirtualMCP{},
		&tables.TableVirtualKeyVirtualMCP{},
	}
}

// TestTenantFKs_MatchUpstreamSchema is the test that makes the edge list
// trustworthy.
//
// Every column name and nullability flag in TenantFKs was read off upstream's
// struct tags by hand, and a typo there does not fail loudly: AddTenantFK would
// error at deploy time on one edge, and a wrong Nullable flag would not error at
// all — it would just mean an edge documented as always-enforced is in fact
// enforced only when the pointer happens to be set.
//
// So the structs are AutoMigrated into a schema of their own and the real columns
// are compared against what we declared. This runs against POSTGRES rather than
// SQLite on purpose: SQLite's introspection reports composite-primary-key columns
// as nullable (measured), which would make the two NOT NULL join-table columns
// look like a mismatch in our list when the mismatch is in the backend.
func TestTenantFKs_MatchUpstreamSchema(t *testing.T) {
	db := newSchemaDB(t, "maas_upstream_shape_test")
	require.NoError(t, db.AutoMigrate(upstreamStructs()...),
		"upstream structs must migrate cleanly; a failure here means the struct set above is incomplete")

	type col struct{ notNull bool }
	actual := map[string]map[string]col{}
	for _, s := range upstreamStructs() {
		stmt := &gorm.Statement{DB: db}
		require.NoError(t, stmt.Parse(s))
		table := stmt.Table
		var rows []struct {
			AttName    string
			AttNotNull bool
		}
		require.NoError(t, db.Raw(`SELECT a.attname AS att_name, a.attnotnull AS att_not_null
			FROM pg_attribute a JOIN pg_class t ON t.oid = a.attrelid
			JOIN pg_namespace n ON n.oid = t.relnamespace
			WHERE t.relname = ? AND n.nspname = ? AND a.attnum > 0 AND NOT a.attisdropped`,
			table, "maas_upstream_shape_test").Scan(&rows).Error)
		require.NotEmpty(t, rows, "no columns found for %s", table)
		actual[table] = map[string]col{}
		for _, r := range rows {
			actual[table][r.AttName] = col{notNull: r.AttNotNull}
		}
	}

	for _, fk := range migrate.TenantFKs {
		t.Run(fk.Name, func(t *testing.T) {
			child, ok := actual[fk.Child]
			require.True(t, ok, "child table %s is not declared by any struct in upstreamStructs", fk.Child)
			parent, ok := actual[fk.Parent]
			require.True(t, ok, "parent table %s is not declared by any struct in upstreamStructs", fk.Parent)

			got, ok := child[fk.ChildColumn]
			require.True(t, ok, "upstream %s has no column %s", fk.Child, fk.ChildColumn)
			require.Equal(t, fk.Nullable, !got.notNull,
				"Nullable is wrong for %s.%s; a wrong flag misstates whether this edge is always enforced",
				fk.Child, fk.ChildColumn)

			_, ok = parent["id"]
			require.True(t, ok, "parent %s has no id column to reference", fk.Parent)
		})
	}
}

// TestTenantFKs_NamesAreUniqueAndFitPostgres guards the reason the names are
// written out by hand instead of generated.
//
// Postgres truncates an identifier past 63 bytes SILENTLY. Generated names on
// these tables would exceed it — "fk_governance_virtual_key_provider_config_keys_
// tenant_governance_virtual_key_provider_configs" is 95 bytes — and two edges on
// the same long-named table can truncate to ONE name, at which point the second
// ADD CONSTRAINT replaces or collides with the first and that table loses an edge
// with nothing reporting it.
func TestTenantFKs_NamesAreUniqueAndFitPostgres(t *testing.T) {
	seen := map[string]string{}
	for _, fk := range migrate.TenantFKs {
		require.LessOrEqual(t, len(fk.Name), 63,
			"%s exceeds Postgres's identifier limit and would be truncated silently", fk.Name)
		prev, dup := seen[fk.Name]
		require.False(t, dup, "%s is used by both %s and %s", fk.Name, prev, fk.Child)
		seen[fk.Name] = fk.Child
	}
	for _, parent := range migrate.ParentTables() {
		require.LessOrEqual(t, len(migrate.ParentKeyName(parent)), 63,
			"parent key name for %s exceeds the identifier limit", parent)
	}
}

// TestTenantFKs_EveryParentIsTenantScoped pins the invariant AddTenantFK checks at
// runtime, so a new edge pointing at an unscoped table fails in a unit test rather
// than at deploy time.
func TestTenantFKs_EveryParentIsTenantScoped(t *testing.T) {
	for _, fk := range migrate.TenantFKs {
		_, ok := migrate.PolicyKindFor(fk.Parent)
		require.True(t, ok, "%s references %s, which has no tenant_id column", fk.Name, fk.Parent)
		_, ok = migrate.PolicyKindFor(fk.Child)
		require.True(t, ok, "%s is declared on %s, which has no tenant_id column", fk.Name, fk.Child)
	}
}

// TestParentTables_IsDerivedFromTheEdgeList checks the set is computed, not
// duplicated. A hand-maintained parent list would be one more thing that can fall
// out of step with TenantFKs, and the symptom would be a missing unique constraint
// discovered as an FK creation failure.
func TestParentTables_IsDerivedFromTheEdgeList(t *testing.T) {
	parents := migrate.ParentTables()
	require.NotEmpty(t, parents)
	for _, fk := range migrate.TenantFKs {
		require.Contains(t, parents, fk.Parent)
	}
	for i := 1; i < len(parents); i++ {
		require.Less(t, parents[i-1], parents[i], "ParentTables must be sorted for stable migration order")
		require.NotEqual(t, parents[i-1], parents[i], "ParentTables must be deduplicated")
	}
}
