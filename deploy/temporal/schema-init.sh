#!/bin/sh
set -eu

if [ "${PM_STACK_MIGRATION_FENCE:-}" != "held-by-pm-stack" ]; then
  echo "temporal schema migration requires the pm-stack deployment fence" >&2
  exit 64
fi

: "${PM_STACK_POSTGRES_HOST:?}"
: "${PM_STACK_POSTGRES_PORT:?}"
: "${PM_STACK_SQL_SERVER_NAME:?}"

export SQL_HOST="$PM_STACK_POSTGRES_HOST"
export SQL_PORT="$PM_STACK_POSTGRES_PORT"
export SQL_PLUGIN=postgres12
export SQL_TLS=true
export SQL_TLS_CA_FILE=/run/secrets/postgres-ca.pem
export SQL_TLS_SERVER_NAME="$PM_STACK_SQL_SERVER_NAME"
export SQL_TLS_DISABLE_HOST_VERIFICATION=false

run_update() {
  database=$1
  user=$2
  password_file=$3
  schema_dir=$4
  target_version=$5

  export SQL_DATABASE="$database"
  export SQL_USER="$user"
  SQL_PASSWORD="$(cat "$password_file")"
  export SQL_PASSWORD

  temporal-sql-tool setup-schema -v 0.0
  temporal-sql-tool update-schema \
    --schema-dir "$schema_dir" \
    --version "$target_version"

  unset SQL_PASSWORD
}

run_update \
  temporal \
  temporal_migrator \
  /run/pm-stack-secrets/temporal-migrator-password \
  /etc/temporal/schema/postgresql/v12/temporal/versioned \
  1.19

run_update \
  temporal_visibility \
  temporal_visibility_migrator \
  /run/pm-stack-secrets/temporal-visibility-migrator-password \
  /etc/temporal/schema/postgresql/v12/visibility/versioned \
  1.14
