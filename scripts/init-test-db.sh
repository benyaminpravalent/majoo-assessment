#!/bin/sh
# Creates the database the integration tests use.
#
# Runs once, on first initialisation of the PostgreSQL data volume, via
# /docker-entrypoint-initdb.d. Keeping test data in a separate database means
# `go test -tags=integration` can TRUNCATE freely without destroying whatever
# you were looking at in development.
set -eu

psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-SQL
	CREATE DATABASE blog_test OWNER $POSTGRES_USER;
SQL

echo "created database blog_test"
