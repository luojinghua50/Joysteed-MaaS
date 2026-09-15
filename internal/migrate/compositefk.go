package migrate

// Composite foreign keys: the constraint that refuses a child row of one tenant
// pointing at a parent row of another.
//
// MAAS_TECH_DESIGN.md §3.4 records why the redundant tenant_id column does not
// cover this on its own. Tenant A's prompt_version can reference tenant B's
// prompt while both rows carry a perfectly correct tenant_id of their own.
// Nothing objects: each row passes its own table's RLS policy, because a policy
// is evaluated per row per table and has no way to see the pair. The pair is
// still a cross-tenant data path. Only the database can refuse the combination,
// and only if it is told which column pairs form one.

import (
	"errors"
	"fmt"
	"sort"

	"gorm.io/gorm"
)

// TenantFK is one parent→child edge that must not cross tenants.
//
// Name is written out rather than generated. Generated names would run past
// Postgres's 63-byte identifier limit for the longer table names here
// (governance_virtual_key_provider_config_keys is 43 bytes before any suffix),
// and truncation is silent — two edges on the same table can truncate to one
// name, at which point the second ADD CONSTRAINT either fails or replaces the
// first. tenantIndexName has the same note for the same reason.
type TenantFK struct {
	Name        string
	Child       string
	ChildColumn string
	Parent      string

	// Nullable records whether ChildColumn is nullable upstream. It changes no
	// DDL — every constraint below is MATCH SIMPLE either way — but it is the
	// difference between an edge that is always enforced and one enforced only
	// when the pointer is set, and that distinction is invisible in the schema.
	// TestTenantFKs_MatchUpstreamSchema compares this field against the real
	// column, so an upstream change from NOT NULL to nullable shows up as a
	// failing test rather than as an edge that quietly stopped covering rows.
	Nullable bool
}

// ParentKeyName is the unique constraint on the parent that the FK references.
//
// Postgres requires a referenced column list to be backed by a unique
// constraint or unique index; the parent's primary key on id alone does not
// qualify for a two-column reference. UNIQUE (tenant_id, id) is redundant as a
// uniqueness claim — id is already unique — but it is what makes the composite
// reference legal, and its index has tenant_id in first position, which is the
// shape R10 asks for.
func ParentKeyName(parent string) string {
	return "uq_" + parent + "_tenant_id_id"
}

// TenantFKs is every edge, read off the upstream structs under
// framework/configstore/tables/ rather than off the audit document.
//
// §3.4 puts the count at "9 child tables"; the real number is 23 edges across 12
// child tables (§7.6.1). The gap is not a miscount in the doc so much as a
// different unit — several tables carry more than one parent pointer, and
// governance_budgets alone has four.
var TenantFKs = []TenantFK{
	// Governance hierarchy. This is the chain a virtual key resolves through, so
	// a cross-tenant edge here is not a stray reference — it is a request funded
	// by the wrong tenant's budget.
	{"fk_teams_tenant_customer", "governance_teams", "customer_id", "governance_customers", true},
	{"fk_vk_tenant_team", "governance_virtual_keys", "team_id", "governance_teams", true},
	{"fk_vk_tenant_customer", "governance_virtual_keys", "customer_id", "governance_customers", true},
	{"fk_vk_tenant_ratelimit", "governance_virtual_keys", "rate_limit_id", "governance_rate_limits", true},
	{"fk_vkpc_tenant_vk", "governance_virtual_key_provider_configs", "virtual_key_id", "governance_virtual_keys", false},
	{"fk_vkpc_tenant_ratelimit", "governance_virtual_key_provider_configs", "rate_limit_id", "governance_rate_limits", true},
	{"fk_vkmc_tenant_vk", "governance_virtual_key_mcp_configs", "virtual_key_id", "governance_virtual_keys", false},
	{"fk_vkpck_tenant_vkpc", "governance_virtual_key_provider_config_keys", "table_virtual_key_provider_config_id", "governance_virtual_key_provider_configs", false},

	// Budgets: four parent pointers, exactly one of which is set per row
	// (upstream models this as a polymorphic owner). Each is covered separately
	// because MATCH SIMPLE skips an edge whose pointer is NULL, so the three
	// unset ones cost nothing.
	{"fk_budget_tenant_vk", "governance_budgets", "virtual_key_id", "governance_virtual_keys", true},
	{"fk_budget_tenant_team", "governance_budgets", "team_id", "governance_teams", true},
	{"fk_budget_tenant_customer", "governance_budgets", "customer_id", "governance_customers", true},
	{"fk_budget_tenant_vkpc", "governance_budgets", "provider_config_id", "governance_virtual_key_provider_configs", true},

	// Prompts.
	{"fk_prompts_tenant_folder", "prompts", "folder_id", "folders", true},
	{"fk_pv_tenant_prompt", "prompt_versions", "prompt_id", "prompts", false},
	{"fk_ps_tenant_prompt", "prompt_sessions", "prompt_id", "prompts", false},
	{"fk_ps_tenant_version", "prompt_sessions", "version_id", "prompt_versions", true},
	{"fk_psm_tenant_prompt", "prompt_session_messages", "prompt_id", "prompts", false},
	{"fk_psm_tenant_session", "prompt_session_messages", "session_id", "prompt_sessions", false},

	// Skills.
	{"fk_sv_tenant_skill", "skill_versions", "skill_id", "skills", false},
	{"fk_sf_tenant_version", "skill_files", "skill_version_id", "skill_versions", false},
	{"fk_sf_tenant_blob", "skill_files", "blob_id", "skill_file_blobs", true},

	// Enterprise MCP tool groups. The physical column is tool_group_id even
	// though the Go field is VirtualMCPID — upstream kept the old name so
	// existing rows are reused (tables/virtualmcp.go:80).
	{"fk_mcptg_vk_tenant_group", "enterprise_mcp_tool_group_virtual_keys", "tool_group_id", "enterprise_mcp_tool_groups", false},
	{"fk_mcptg_vk_tenant_vk", "enterprise_mcp_tool_group_virtual_keys", "virtual_key_id", "governance_virtual_keys", false},
}

// Two edges that exist in the schema and are deliberately NOT in the list above.
// Both are recorded here because "absent from the list" and "considered and
// excluded" are indistinguishable otherwise, and the first one would look like
// an oversight to anyone counting child tables.
//
// governance_virtual_key_provider_config_keys.table_key_id → config_keys
//
//	config_keys is C-class: tenant_id IS NULL means the shared platform pool
//	(D1). A composite FK would demand the child's tenant_id equal the parent's,
//	so every tenant referencing a platform key — the entire point of the shared
//	pool — would be refused. Cross-tenant reference of another tenant's BYOK key
//	is left to config_keys' PolicyPool policy, which makes such a row unreadable.
//
// governance_budgets.model_config_id → governance_model_configs
//
//	governance_model_configs is D-class. The audit (§5) says it needs tenant_id,
//	but it is not in BClassTables and so has no such column and no policy — there
//	is nothing to reference. See §7.6.2; this is an open gap, not a decision.

// ParentTables returns every table referenced by an edge, sorted — the set that
// needs a ParentKeyName constraint. Derived rather than written out so it cannot
// fall out of step with TenantFKs.
func ParentTables() []string {
	seen := make(map[string]struct{}, len(TenantFKs))
	out := make([]string, 0, len(TenantFKs))
	for _, fk := range TenantFKs {
		if _, ok := seen[fk.Parent]; ok {
			continue
		}
		seen[fk.Parent] = struct{}{}
		out = append(out, fk.Parent)
	}
	sort.Strings(out)
	return out
}

// ErrTenantColumnNullable reports that a composite FK was about to be created
// over a child whose tenant_id is still nullable.
//
// Refused, because the constraint would be created successfully and enforce
// nothing on the rows that matter. See AddTenantFK.
var ErrTenantColumnNullable = errors.New("migrate: child tenant_id is still nullable")

// AddParentTenantKey adds the UNIQUE (tenant_id, id) the composite FKs
// reference.
//
// Safe before the backfill: UNIQUE permits multiple NULLs, and id is already
// unique on its own, so no pair can collide whatever tenant_id holds. That makes
// this the one step of the three that has no ordering requirement — it is
// sequenced before the FKs only because a reference to a constraint that does not
// exist yet fails.
func AddParentTenantKey(tx *gorm.DB, parent string) error {
	if err := checkIdentifier(parent); err != nil {
		return err
	}
	name := ParentKeyName(parent)
	if err := checkIdentifier(name); err != nil {
		return err
	}
	// No IF NOT EXISTS for ADD CONSTRAINT in Postgres, so guard on the catalog.
	// This is check-then-act and therefore racy under two concurrent runners; the
	// loser gets SQLSTATE 42710 and its transaction aborts, which the migrator
	// surfaces as a failed migration to be re-run rather than as a corrupt schema.
	// The alternative — DROP then ADD — would take an ACCESS EXCLUSIVE lock and
	// briefly leave the parent with no unique key while FKs reference it.
	exists, err := constraintExists(tx, parent, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT %s UNIQUE (%s, id)`, parent, name, TenantColumn)).Error; err != nil {
		return fmt.Errorf("migrate: add %s to %s: %w", name, parent, err)
	}
	return nil
}

// DropParentTenantKey reverses AddParentTenantKey.
func DropParentTenantKey(tx *gorm.DB, parent string) error {
	if err := checkIdentifier(parent); err != nil {
		return err
	}
	name := ParentKeyName(parent)
	if err := checkIdentifier(name); err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s`, parent, name)).Error; err != nil {
		return fmt.Errorf("migrate: drop %s from %s: %w", name, parent, err)
	}
	return nil
}

// AddTenantFK creates one composite foreign key, refusing if the child's
// tenant_id is still nullable.
//
// That refusal is the whole reason this function is not a one-line Exec, and it
// contradicts the migration order §3.4 and §7.4.6 both give ("backfill → composite
// FK → SET NOT NULL"). The order has to be backfill → SET NOT NULL → FK, because
// of what MATCH SIMPLE means.
//
// Postgres's default matching for a multi-column foreign key is MATCH SIMPLE:
// if ANY referencing column is NULL, the constraint is considered satisfied and
// no lookup happens. So while tenant_id is nullable, every unattributed row —
// precisely the rows a half-finished backfill leaves behind — is exempt from the
// constraint. The ALTER TABLE succeeds, the constraint appears in the catalog,
// and it covers only the rows that were already fine.
//
// MATCH FULL is not the fix. It permits all-NULL and all-non-NULL and rejects
// mixtures, which for a legitimately nullable pointer (prompts.folder_id on an
// unfoldered prompt, with tenant_id set) rejects a valid row. So the shape stays
// MATCH SIMPLE and the NULL is removed from tenant_id first instead.
//
// ON DELETE is left at NO ACTION deliberately. Upstream already declares its own
// single-column FK on each of these edges with its own referential action
// (mostly CASCADE, SET NULL on prompt_sessions.version_id and skill_files.blob_id),
// and that FK still fires: by the time this constraint is checked at end of
// statement, the cascade has already removed or nulled the referencing row.
// Duplicating the action would mean two constraints racing to perform the same
// delete; leaving it NO ACTION means that if upstream's FK is ever dropped, parent
// deletes start failing loudly instead of orphaning rows silently.
func AddTenantFK(tx *gorm.DB, fk TenantFK) error {
	for _, id := range []string{fk.Name, fk.Child, fk.ChildColumn, fk.Parent} {
		if err := checkIdentifier(id); err != nil {
			return err
		}
	}
	if _, ok := PolicyKindFor(fk.Parent); !ok {
		// A parent with no policy has no tenant_id column to reference. Caught
		// here rather than left to Postgres because the failure it produces
		// ("column tenant_id does not exist") does not say which of the two
		// tables is missing it.
		return fmt.Errorf("migrate: FK %s references %s, which is not tenant-scoped", fk.Name, fk.Parent)
	}

	notNull, err := columnIsNotNull(tx, fk.Child, TenantColumn)
	if err != nil {
		return err
	}
	if !notNull {
		return fmt.Errorf("%w: %s.%s must be NOT NULL before %s is created, or MATCH SIMPLE leaves every unattributed row unchecked",
			ErrTenantColumnNullable, fk.Child, TenantColumn, fk.Name)
	}

	exists, err := constraintExists(tx, fk.Child, fk.Name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s, %s) REFERENCES %s (%s, id) MATCH SIMPLE`,
		fk.Child, fk.Name, TenantColumn, fk.ChildColumn, fk.Parent, TenantColumn)).Error; err != nil {
		return fmt.Errorf("migrate: add %s on %s: %w", fk.Name, fk.Child, err)
	}
	return nil
}

// DropTenantFK reverses AddTenantFK.
func DropTenantFK(tx *gorm.DB, fk TenantFK) error {
	if err := checkIdentifier(fk.Child); err != nil {
		return err
	}
	if err := checkIdentifier(fk.Name); err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s`, fk.Child, fk.Name)).Error; err != nil {
		return fmt.Errorf("migrate: drop %s from %s: %w", fk.Name, fk.Child, err)
	}
	return nil
}

// constraintExists reports whether a named constraint is present on a table.
//
// Reads pg_constraint joined to pg_class rather than information_schema:
// information_schema.table_constraints only shows constraints the current role
// has privileges on, so a role that can ALTER the table but does not own it can
// read "absent" for a constraint that is present, and then fail on the ADD.
func constraintExists(tx *gorm.DB, table, name string) (bool, error) {
	var n int64
	err := tx.Raw(`SELECT count(*) FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = ? AND c.conname = ?`, table, name).Scan(&n).Error
	if err != nil {
		return false, fmt.Errorf("migrate: look up constraint %s on %s: %w", name, table, err)
	}
	return n > 0, nil
}

// TenantColumnIsNotNull reports whether a table's tenant_id has been tightened.
//
// The operator-facing half of the phase-3 gate: it answers "has this table been
// through pass 1" without having to read pg_attribute by hand, and it is what
// distinguishes a composite FK that covers every row from one that covers only the
// attributed ones (see AddTenantFK).
func TenantColumnIsNotNull(tx *gorm.DB, table string) (bool, error) {
	if err := checkIdentifier(table); err != nil {
		return false, err
	}
	return columnIsNotNull(tx, table, TenantColumn)
}

// columnIsNotNull reports whether a column is declared NOT NULL.
//
// Returns an error rather than false when the column is absent. False would be
// the same answer this function gives for "nullable", and the two lead to
// opposite conclusions: a nullable column means the migration order is wrong, a
// missing column means the phase-1 column migration has not run.
func columnIsNotNull(tx *gorm.DB, table, column string) (bool, error) {
	var got []bool
	err := tx.Raw(`SELECT a.attnotnull FROM pg_attribute a
		JOIN pg_class t ON t.oid = a.attrelid
		WHERE t.relname = ? AND a.attname = ? AND a.attnum > 0 AND NOT a.attisdropped`,
		table, column).Scan(&got).Error
	if err != nil {
		return false, fmt.Errorf("migrate: read nullability of %s.%s: %w", table, column, err)
	}
	if len(got) == 0 {
		return false, fmt.Errorf("migrate: %s has no column %s (has its column migration run?)", table, column)
	}
	return got[0], nil
}
