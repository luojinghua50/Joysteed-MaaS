-- Bifrost / MaaS Postgres bootstrap
--
-- WHAT THIS DOES: creates the databases, roles and grants that must exist
-- BEFORE Bifrost's first boot.
--
-- WHAT THIS DELIBERATELY DOES NOT DO: create tables. Bifrost's schema is
-- generated at runtime from GORM struct tags -- newPostgresConfigStore opens a
-- throwaway migration pool, runs triggerMigrations -> runMigrationSteps, and
-- each step calls migrator.CreateTable / AddColumnIfNotExists. The 43 table
-- structs under framework/configstore/tables/ are the single source of truth.
-- Hand-written table DDL would become a second source of truth AND would defeat
-- the pending-migration detection (areThereAnyPendingMigrations /
-- pendingMigrationStepIDs): tables would exist while the migration-metadata
-- table has no matching IDs, leaving later upgrades in an undefined state.
--
-- HOW TO RUN (as a superuser, against the maintenance database):
--
--   psql -h HOST -p PORT -U postgres -d postgres \
--        -v config_password="$(cat config_pw.txt)" \
--        -v logs_password="$(cat logs_pw.txt)" \
--        -f 01_bootstrap.sql
--
-- Idempotent: safe to re-run. Existing roles/databases are left untouched
-- (including their passwords -- rotate those separately with ALTER ROLE).
--
-- Requires psql (uses \gexec and \c). Not runnable through a plain SQL client.

\set ON_ERROR_STOP on

-- Two databases, not one. configstore and logstore have very different write
-- volume, vacuum pressure and retention profiles; splitting them keeps a log
-- write storm from degrading config reads. Table names do not collide, so a
-- single shared database would also work -- this is an operational choice.
--
-- Two roles, not one, for least privilege: config_keys holds provider
-- credentials including tenant BYOK, so a leaked logs credential must not be
-- able to read it.
--
-- NOSUPERUSER + NOBYPASSRLS are load-bearing, not hygiene. A superuser or
-- BYPASSRLS role ignores row-level security entirely and is not constrained
-- even by FORCE ROW LEVEL SECURITY -- policies stay in place and silently stop
-- filtering. See MAAS_TECH_DESIGN.md R31 / R37.

SELECT format(
    'CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION',
    'bifrost_config_app', :'config_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'bifrost_config_app')
\gexec

SELECT format(
    'CREATE ROLE %I LOGIN PASSWORD %L NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION',
    'bifrost_logs_app', :'logs_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'bifrost_logs_app')
\gexec

-- CREATE DATABASE cannot run inside a transaction block; psql's default
-- autocommit is what makes this work. Owner is the app role: Bifrost runs its
-- own migrations, so it needs CREATE on the schema it migrates.
SELECT format('CREATE DATABASE %I OWNER %I ENCODING ''UTF8''',
              'bifrost_config', 'bifrost_config_app')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'bifrost_config')
\gexec

SELECT format('CREATE DATABASE %I OWNER %I ENCODING ''UTF8''',
              'bifrost_logs', 'bifrost_logs_app')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'bifrost_logs')
\gexec

-- Config database ----------------------------------------------------------
\c bifrost_config

-- Explicit rather than relying on defaults, because the default changed: PG15+
-- revokes CREATE on schema public from PUBLIC, PG14 and earlier grant it. The
-- app role needs CREATE because it runs its own DDL.
GRANT USAGE, CREATE ON SCHEMA public TO bifrost_config_app;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON DATABASE bifrost_config FROM PUBLIC;
GRANT CONNECT ON DATABASE bifrost_config TO bifrost_config_app;

-- Logs database ------------------------------------------------------------
\c bifrost_logs

GRANT USAGE, CREATE ON SCHEMA public TO bifrost_logs_app;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON DATABASE bifrost_logs FROM PUBLIC;
GRANT CONNECT ON DATABASE bifrost_logs TO bifrost_logs_app;

-- Verification -------------------------------------------------------------
-- Both roles must show f/f. A t in either column means RLS will not constrain
-- that role no matter what policies M1 installs, and the isolation preflight
-- (internal/rls) will refuse to start.
\c postgres
SELECT rolname, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole
FROM pg_roles
WHERE rolname IN ('bifrost_config_app', 'bifrost_logs_app')
ORDER BY rolname;

-- NEXT STEPS (deliberately not in this file):
--
-- 1. Start Bifrost once with the config below. It creates every table.
--
-- 2. Only then can RLS be enabled -- ENABLE/FORCE ROW LEVEL SECURITY and
--    CREATE POLICY need the tables to exist. That belongs in M1's migration
--    code (run via migrateOnFreshFn), not a hand-run script: the policy set is
--    derived per-table from the MAAS_TABLE_AUDIT.md ownership mapping, and it
--    has to re-run for tables added by future upstream upgrades.
--
-- 3. Set logs_store matview_refresh_interval to "off" -- materialized views are
--    not constrained by RLS and cannot be made to be (R37 / D14).
--
-- KNOWN LIMITATION: the app role owns the tables it creates. Two consequences.
-- (a) FORCE ROW LEVEL SECURITY is mandatory, not optional -- a table owner is
--     exempt from its own table's RLS unless FORCE is set.
-- (b) Even with FORCE, the owner can ALTER TABLE ... NO FORCE or DROP POLICY.
--     Isolation therefore holds against SQL injection and missing-predicate
--     bugs, but not against a compromised app credential. Splitting migration
--     and runtime into separate roles would close this, but Bifrost's config
--     exposes one credential per store, so it needs two config files and a
--     migrate-then-restart deploy step. Not done here; recorded as a gap.

