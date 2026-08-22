-- +goose Up
ALTER TABLE tenant ADD CONSTRAINT tenant_name_key UNIQUE (name);

-- +goose Down
ALTER TABLE tenant DROP CONSTRAINT tenant_name_key;
