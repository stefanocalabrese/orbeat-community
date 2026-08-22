-- Keycloak's schema lives in its own database within the same Postgres instance.
-- Runs once, on first initialization of the data volume.
CREATE DATABASE keycloak;
