package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/maximhq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

// CompositeIndex describes the tenant-leading index used by a tenant-visible
// lookup. The tenant discriminator is always first; the remaining columns are
// chosen from the actual lookup predicates in configstore/rdb.go. Names are
// explicit and short because PostgreSQL silently truncates long identifiers.
type CompositeIndex struct {
	Name   string
	Table  string
	Column string
}

// CompositeIndexes is intentionally explicit rather than generated from Go
// structs. A generated index tends to follow the primary key even when the hot
// query is by a foreign key, and it would miss new query paths silently.
var CompositeIndexes = []CompositeIndex{
	{"idx_gc_tenant_id", "governance_customers", "id"},
	{"idx_gt_tenant_customer", "governance_teams", "customer_id"},
	{"idx_gvk_tenant_hash", "governance_virtual_keys", "value_hash"},
	{"idx_gvkpc_tenant_vk", "governance_virtual_key_provider_configs", "virtual_key_id"},
	{"idx_gvkpck_tenant_vkpc", "governance_virtual_key_provider_config_keys", "table_virtual_key_provider_config_id"},
	{"idx_gvkmc_tenant_vk", "governance_virtual_key_mcp_configs", "virtual_key_id"},
	{"idx_gb_tenant_vk", "governance_budgets", "virtual_key_id"},
	{"idx_grl_tenant_id", "governance_rate_limits", "id"},
	{"idx_prompts_tenant_id", "prompts", "id"},
	{"idx_pv_tenant_prompt", "prompt_versions", "prompt_id"},
	{"idx_ps_tenant_prompt", "prompt_sessions", "prompt_id"},
	{"idx_psm_tenant_prompt", "prompt_session_messages", "prompt_id"},
	{"idx_folders_tenant_id", "folders", "id"},
	{"idx_skills_tenant_id", "skills", "id"},
	{"idx_sv_tenant_skill", "skill_versions", "skill_id"},
	{"idx_sf_tenant_version", "skill_files", "skill_version_id"},
	{"idx_sfb_tenant_id", "skill_file_blobs", "id"},
	{"idx_jobs_tenant_vk", "batch_jobs", "virtual_key_id"},
	{"idx_tokens_tenant_scope", "temp_tokens", "scope"},
	{"idx_mof_tenant_vk", "mcp_oauth_flows", "virtual_key_id"},
	{"idx_mot_tenant_vk", "mcp_oauth_tokens", "virtual_key_id"},
	{"idx_mphf_tenant_vk", "mcp_per_user_header_flows", "virtual_key_id"},
	{"idx_mphc_tenant_vk", "mcp_per_user_header_credentials", "virtual_key_id"},
	{"idx_ous_tenant_id", "oauth_user_sessions", "id"},
	{"idx_out_tenant_vk", "oauth_user_tokens", "virtual_key_id"},
	{"idx_emctg_tenant_id", "enterprise_mcp_tool_groups", "id"},
	{"idx_emctgvk_tenant_group", "enterprise_mcp_tool_group_virtual_keys", "tool_group_id"},
	{"idx_keys_tenant_key", CClassTable, "key_id"},
	{"idx_sessions_tenant_hash", SessionsTable, "token_hash"},
}

func compositeIndexMigration(index CompositeIndex) *migrator.Migration {
	return &migrator.Migration{
		ID: "20260915-01-index-" + index.Name,
		Migrate: func(tx *gorm.DB) error {
			for _, id := range []string{index.Name, index.Table, index.Column} {
				if err := checkIdentifier(id); err != nil {
					return err
				}
			}
			if err := tx.Exec(fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s, %s)", index.Name, index.Table, TenantColumn, index.Column)).Error; err != nil {
				return fmt.Errorf("migrate: add composite index %s: %w", index.Name, err)
			}
			return nil
		},
		Rollback: func(tx *gorm.DB) error {
			if err := checkIdentifier(index.Name); err != nil {
				return err
			}
			if err := tx.Exec("DROP INDEX IF EXISTS " + index.Name).Error; err != nil {
				return fmt.Errorf("migrate: drop composite index %s: %w", index.Name, err)
			}
			return nil
		},
	}
}

func compositeIndexMigrations() []*migrator.Migration {
	out := make([]*migrator.Migration, 0, len(CompositeIndexes))
	for _, index := range CompositeIndexes {
		out = append(out, compositeIndexMigration(index))
	}
	return out
}

// IndexMigrations returns the additive R10 index set. It is intentionally a
// separate set from Migrations: existing policy migrations must remain stable,
// while operators can deploy query-shape indexes independently and online.
func IndexMigrations() []*migrator.Migration { return compositeIndexMigrations() }

// ApplyIndexes applies the R10 set against a migration connection.
func ApplyIndexes(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("migrate: db is nil")
	}
	opts := *migrator.DefaultOptions
	opts.TableName = migrationTable
	if err := migrator.New(db.WithContext(ctx), &opts, IndexMigrations()).Migrate(); err != nil {
		return fmt.Errorf("migrate: apply tenant composite indexes: %w", err)
	}
	return nil
}

// RunIndexes applies R10 through the upstream throwaway migration pool and
// refreshes the runtime pool so prepared plans see the new indexes.
func RunIndexes(ctx context.Context, store Store) error {
	if store == nil {
		return errors.New("migrate: store is nil")
	}
	if err := store.RunMigration(ctx, func(ctx context.Context, db *gorm.DB) error {
		return ApplyIndexes(ctx, db)
	}); err != nil {
		return err
	}
	if err := store.RefreshConnectionPool(ctx); err != nil {
		return fmt.Errorf("migrate: refresh connection pool after indexes: %w", err)
	}
	return nil
}
