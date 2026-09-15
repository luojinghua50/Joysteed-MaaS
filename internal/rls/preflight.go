// Package rls enforces the deployment preconditions Postgres row-level security
// silently depends on.
//
// This exists because of the sharpest finding in MAAS_TECH_DESIGN.md §9.2.3:
// RLS can be fully configured — policies created, ENABLE ROW LEVEL SECURITY set —
// and still enforce nothing, with no error raised anywhere. Three conditions
// each cause it independently:
//
//   - the connecting role is a superuser (FORCE does not constrain superusers)
//   - the connecting role holds BYPASSRLS (same)
//   - the table lacks FORCE ROW LEVEL SECURITY while the role owns it
//
// The third is the default state for Bifrost: framework/configstore/postgres.go
// builds the migration pool and the runtime pool from one config, so the runtime
// role is the table owner. The first is the default for a container Postgres.
//
// A failure mode that raises no error cannot be caught by tests that assert on
// errors, so it has to be checked explicitly at startup and refused.
//
// The dialect itself is checked first, for the same reason and then some. RLS is
// a Postgres feature, so on any other backend every precondition below is moot —
// but the cost of the wrong backend is not limited to isolation. Bifrost's own
// code degrades silently off Postgres: transports/bifrost-http/handlers's
// dbForUpdate returns the query unmodified rather than adding FOR UPDATE, so a
// call site written to take a row lock takes none, with no error. Its one caller
// is the virtual-key update transaction, which reloads the key together with its
// budgets, rate limits and provider configs and reconciles them — including
// deciding which rows to delete. What form the missing lock takes on another
// backend (a lost update, or a busy error) depends on that backend's own
// locking and is not measured here; either way the lock the code asks for is
// not taken. See MAAS_TECH_DESIGN.md R36.
package rls

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// RequiredDialect is the only backend on which tenant isolation can be
// enforced, and — independently of isolation — the only one on which Bifrost's
// own row-locking and batched-write paths do what their call sites assume.
const RequiredDialect = "postgres"

// Finding is one failed precondition.
type Finding struct {
	// Check names the precondition, e.g. "role.superuser".
	Check string
	// Detail explains what was observed.
	Detail string
	// Remedy is the concrete fix.
	Remedy string
}

func (f Finding) String() string {
	return fmt.Sprintf("[%s] %s — fix: %s", f.Check, f.Detail, f.Remedy)
}

// Result is the outcome of a preflight run.
type Result struct {
	// Dialect is the backend actually connected to, recorded even when it is
	// the wrong one so the failure names what it found.
	Dialect string
	// Role is empty when the dialect check short-circuited, since reading
	// current_user is itself Postgres-specific.
	Role     string
	Findings []Finding
}

// OK reports whether every precondition held.
func (r *Result) OK() bool { return len(r.Findings) == 0 }

// Err returns a single error naming every failure, or nil when all passed.
//
// All findings are reported together rather than failing on the first, because
// an operator fixing these is editing database grants and migrations: learning
// about one problem per restart turns a five-minute fix into an afternoon.
func (r *Result) Err() error {
	if r.OK() {
		return nil
	}
	var b strings.Builder
	// Role is empty when the dialect check short-circuited, so name the dialect
	// instead — %q on an empty role would report a failure for role "".
	subject := fmt.Sprintf("role %q", r.Role)
	if r.Role == "" {
		subject = fmt.Sprintf("dialect %q", r.Dialect)
	}
	fmt.Fprintf(&b, "rls preflight failed for %s: tenant isolation would NOT be enforced (%d problems)",
		subject, len(r.Findings))
	for _, f := range r.Findings {
		b.WriteString("\n  - ")
		b.WriteString(f.String())
	}
	b.WriteString("\n\nRefusing to start: each problem above disables RLS silently, " +
		"without any query error. See MAAS_TECH_DESIGN.md §9.2.3.")
	return fmt.Errorf("%s", b.String())
}

// Check verifies every precondition for the given tenant tables.
//
// tenantTables is supplied by the caller rather than discovered, because "which
// tables are tenant-scoped" is a product decision recorded in
// MAAS_TABLE_AUDIT.md, not something inferable from the schema. Passing an
// incomplete list is itself a hazard, so ExpectedTableCount can be used to pin
// it against the audit.
func Check(ctx context.Context, db *gorm.DB, tenantTables []string) (*Result, error) {
	res := &Result{Dialect: db.Dialector.Name()}

	// Checked first, and short-circuiting. Every check below reads a pg_* catalog,
	// so on another backend they would fail as SQL errors — reporting "no such
	// table: pg_roles" instead of "this backend cannot enforce isolation". The
	// wrong-dialect verdict is a finding, not a Check error: the connection is
	// healthy and the answer is known, which is exactly what a finding is for.
	if res.Dialect != RequiredDialect {
		res.Findings = append(res.Findings, Finding{
			Check: "dialect.not_postgres",
			Detail: fmt.Sprintf("connected to %q, but tenant isolation requires %q; "+
				"row-level security does not exist on this backend, and Bifrost's own "+
				"dbForUpdate silently omits FOR UPDATE off Postgres, so the virtual-key "+
				"update transaction takes no row lock and raises no error (R36)",
				res.Dialect, RequiredDialect),
			Remedy: `set config_store.type and logs_store.type to "postgres" ` +
				"(both are selected independently; configuring one leaves the other on SQLite)",
		})
		return res, nil
	}

	if err := db.WithContext(ctx).Raw(`SELECT current_user`).Scan(&res.Role).Error; err != nil {
		return nil, fmt.Errorf("rls preflight: read current_user: %w", err)
	}

	if err := checkRole(ctx, db, res); err != nil {
		return nil, err
	}
	if err := checkTables(ctx, db, tenantTables, res); err != nil {
		return nil, err
	}

	sort.SliceStable(res.Findings, func(i, j int) bool {
		return res.Findings[i].Check < res.Findings[j].Check
	})
	return res, nil
}

// Enforce runs Check and returns an error unless every precondition held.
// Call this during startup, before serving traffic.
func Enforce(ctx context.Context, db *gorm.DB, tenantTables []string) error {
	res, err := Check(ctx, db, tenantTables)
	if err != nil {
		return err
	}
	return res.Err()
}

type roleAttrs struct {
	IsSuperuser bool `gorm:"column:is_superuser"`
	BypassRLS   bool `gorm:"column:bypass_rls"`
}

func checkRole(ctx context.Context, db *gorm.DB, res *Result) error {
	var attrs roleAttrs
	err := db.WithContext(ctx).Raw(`
		SELECT rolsuper AS is_superuser, rolbypassrls AS bypass_rls
		FROM pg_roles
		WHERE rolname = current_user`).Scan(&attrs).Error
	if err != nil {
		return fmt.Errorf("rls preflight: read role attributes: %w", err)
	}

	if attrs.IsSuperuser {
		res.Findings = append(res.Findings, Finding{
			Check: "role.superuser",
			Detail: fmt.Sprintf("role %q is a superuser; superusers bypass RLS entirely, "+
				"and FORCE ROW LEVEL SECURITY does not constrain them", res.Role),
			Remedy: "connect as a dedicated non-superuser role (see docs/rls-setup)",
		})
	}
	if attrs.BypassRLS {
		res.Findings = append(res.Findings, Finding{
			Check:  "role.bypassrls",
			Detail: fmt.Sprintf("role %q holds BYPASSRLS; every policy is ignored", res.Role),
			Remedy: fmt.Sprintf("ALTER ROLE %s NOBYPASSRLS", quoteIdent(res.Role)),
		})
	}

	// A role may also reach BYPASSRLS by SET ROLE to a role that holds it.
	// Attributes are not inherited through membership, so plain membership is
	// not itself a bypass — but it is a standing capability worth surfacing,
	// because it turns "isolation holds" into "isolation holds unless someone
	// runs SET ROLE".
	var grantors []string
	err = db.WithContext(ctx).Raw(`
		SELECT r.rolname
		FROM pg_auth_members m
		JOIN pg_roles r ON r.oid = m.roleid
		JOIN pg_roles member ON member.oid = m.member
		WHERE member.rolname = current_user
		  AND (r.rolsuper OR r.rolbypassrls)`).Scan(&grantors).Error
	if err != nil {
		return fmt.Errorf("rls preflight: read role memberships: %w", err)
	}
	if len(grantors) > 0 {
		sort.Strings(grantors)
		res.Findings = append(res.Findings, Finding{
			Check: "role.bypass_reachable",
			Detail: fmt.Sprintf("role %q is a member of RLS-exempt role(s) %s; "+
				"SET ROLE to any of them disables every policy for that session",
				res.Role, strings.Join(grantors, ", ")),
			Remedy: fmt.Sprintf("REVOKE %s FROM %s",
				quoteIdent(grantors[0]), quoteIdent(res.Role)),
		})
	}
	return nil
}

type tableState struct {
	Name     string `gorm:"column:name"`
	Enabled  bool   `gorm:"column:enabled"`
	Forced   bool   `gorm:"column:forced"`
	Policies int    `gorm:"column:policies"`
	Exists   bool   `gorm:"column:exists"`
}

func checkTables(ctx context.Context, db *gorm.DB, tenantTables []string, res *Result) error {
	if len(tenantTables) == 0 {
		res.Findings = append(res.Findings, Finding{
			Check:  "tables.empty",
			Detail: "no tenant tables supplied, so nothing was verified",
			Remedy: "pass the tenant-scoped table list from MAAS_TABLE_AUDIT.md",
		})
		return nil
	}

	states := make(map[string]tableState, len(tenantTables))
	for _, name := range tenantTables {
		var st tableState
		err := db.WithContext(ctx).Raw(`
			SELECT c.relname AS name,
			       c.relrowsecurity AS enabled,
			       c.relforcerowsecurity AS forced,
			       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid) AS policies,
			       true AS exists
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind = 'r'
			  AND n.nspname = current_schema()
			  AND c.relname = ?`, name).Scan(&st).Error
		if err != nil {
			return fmt.Errorf("rls preflight: inspect table %q: %w", name, err)
		}
		st.Name = name
		states[name] = st
	}

	for _, name := range tenantTables {
		st := states[name]
		switch {
		case !st.Exists:
			res.Findings = append(res.Findings, Finding{
				Check:  "table.missing",
				Detail: fmt.Sprintf("table %q was not found in the current schema", name),
				Remedy: "run migrations first, or correct the tenant table list",
			})
			continue
		case !st.Enabled:
			res.Findings = append(res.Findings, Finding{
				Check:  "table.rls_disabled",
				Detail: fmt.Sprintf("table %q has no row-level security enabled", name),
				Remedy: fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", quoteIdent(name)),
			})
		}
		if st.Enabled && !st.Forced {
			res.Findings = append(res.Findings, Finding{
				Check: "table.not_forced",
				Detail: fmt.Sprintf("table %q has RLS enabled but not FORCED; its owner "+
					"is exempt, and the runtime role IS the owner in Bifrost's default "+
					"single-config pool setup", name),
				Remedy: fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", quoteIdent(name)),
			})
		}
		if st.Exists && st.Policies == 0 {
			res.Findings = append(res.Findings, Finding{
				Check: "table.no_policy",
				Detail: fmt.Sprintf("table %q has row-level security but zero policies; "+
					"with RLS on and no policy the table reads as empty, which fails "+
					"closed but breaks the feature", name),
				Remedy: fmt.Sprintf("CREATE POLICY ... ON %s", quoteIdent(name)),
			})
		}
	}
	return nil
}

// quoteIdent renders an identifier for the remedy strings. These are shown to
// operators, never executed, but a broken suggestion is still a bug.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
