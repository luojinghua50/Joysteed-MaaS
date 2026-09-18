#!/bin/sh
set -eu

: "${POSTGRES_USER:?POSTGRES_USER is required}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
: "${BIFROST_CONFIG_PASSWORD:?BIFROST_CONFIG_PASSWORD is required}"
: "${BIFROST_LOGS_PASSWORD:?BIFROST_LOGS_PASSWORD is required}"

export PGPASSWORD="${POSTGRES_PASSWORD}"

exec psql \
  --host=postgres \
  --username="${POSTGRES_USER}" \
  --dbname=postgres \
  --set=config_password="${BIFROST_CONFIG_PASSWORD}" \
  --set=logs_password="${BIFROST_LOGS_PASSWORD}" \
  --file=/bootstrap/01_bootstrap.sql
