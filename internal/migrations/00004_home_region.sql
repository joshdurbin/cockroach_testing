-- +goose NO TRANSACTION
-- +goose Up

-- home_region is now included in tenants from 00001_initial_schema.sql.
-- This migration is a no-op kept only to preserve the goose version sequence
-- for any existing deployments that already have version 4 recorded.
SELECT 1;

-- +goose Down
SELECT 1;
