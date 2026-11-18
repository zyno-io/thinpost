-- Runs once, as the PostgreSQL superuser, on first start of the data volume.
-- Passwords come from the pki-init volume through psql variables.
CREATE ROLE thinpost_owner LOGIN PASSWORD :'owner_pw';
CREATE ROLE thinpost_runtime LOGIN PASSWORD :'runtime_pw';
CREATE DATABASE thinpost OWNER thinpost_owner;
