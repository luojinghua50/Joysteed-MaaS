package migrate

// The tenant-scoped table list, taken from MAAS_TABLE_AUDIT.md and verified
// against the 56 TableName() values actually declared under
// framework/configstore/tables/.
//
// This list is passed to rls.Enforce as well as driving the migration, which is
// why it lives in one place: preflight's tables.empty finding exists because
// validating zero tables and reporting success is the most dangerous false
// positive available, and two separate lists would let the migration widen a
// table that preflight never checks.
//
// Deliberately NOT derived by scanning the schema. Which tables belong to a
// tenant is a product decision recorded in the audit; a table added by a future
// upstream upgrade must fail to be covered loudly rather than be silently
// classified by a heuristic.

// BClassTables are the 27 strictly tenant-owned tables (audit §3). Each gets a
// tenant_id column, a PolicyStrict policy, and — after its backfill — NOT NULL.
var BClassTables = []string{
	// §3.1 governance hierarchy. The core isolation chain: this is what a virtual
	// key resolves through, so it is the path that decides whether a request is
	// attributed and limited correctly.
	"governance_customers",
	"governance_teams",
	"governance_virtual_keys",
	"governance_virtual_key_provider_configs",
	"governance_virtual_key_provider_config_keys",
	"governance_virtual_key_mcp_configs",
	"governance_budgets",
	"governance_rate_limits",

	// §3.2 prompt / skill / files. prompts and skills are the two modules already
	// using upstream's ScopedDB, which makes them the reference implementation for
	// the remaining unscoped queries rather than new ground.
	"prompts",
	"prompt_versions",
	"prompt_sessions",
	"prompt_session_messages",
	"folders",
	"skills",
	"skill_versions",
	"skill_files",
	"skill_file_blobs",

	// §3.3 jobs / tokens / MCP user state. Several of these hold credentials
	// (mcp_per_user_header_credentials, oauth_user_tokens), which is why their
	// backfill cannot use a default tenant: misattribution here is a credential
	// leak, not untidy data.
	"batch_jobs",
	"temp_tokens",
	"mcp_oauth_flows",
	"mcp_oauth_tokens",
	"mcp_per_user_header_flows",
	"mcp_per_user_header_credentials",
	"oauth_user_sessions",
	"oauth_user_tokens",
	"enterprise_mcp_tool_groups",
	"enterprise_mcp_tool_group_virtual_keys",
}

// CClassTable is the one table whose tenant_id is legitimately nullable: NULL
// means the shared platform pool (D1 dual mode). It gets PolicyPool and never
// NOT NULL.
const CClassTable = "config_keys"

// SessionsTable is handled on its own (audit §6.1). It needs three columns rather
// than one and PolicyExclusive rather than PolicyStrict.
const SessionsTable = "sessions"

// TenantScopedTables is every table carrying a tenant_id, for passing to
// rls.Enforce. Built rather than written out so it cannot fall out of step with
// the lists above.
func TenantScopedTables() []string {
	out := make([]string, 0, len(BClassTables)+2)
	out = append(out, BClassTables...)
	out = append(out, CClassTable, SessionsTable)
	return out
}

// PolicyKindFor reports which policy shape a table takes.
//
// A table absent from every list returns false rather than defaulting to
// PolicyStrict. Defaulting would mean a table nobody classified silently gets a
// policy, and for a platform-level table (A-class) a strict tenant policy makes
// every row invisible to everyone — a total functional outage whose cause is a
// missing entry in a list.
func PolicyKindFor(table string) (PolicyKind, bool) {
	switch table {
	case CClassTable:
		return PolicyPool, true
	case SessionsTable:
		return PolicyExclusive, true
	}
	for _, t := range BClassTables {
		if t == table {
			return PolicyStrict, true
		}
	}
	return 0, false
}
