-- Create the Keycloak database (Phase 1+).
-- This file is mounted into the postgres container at /docker-entrypoint-initdb.d/
-- and runs once on first boot (when the data volume is empty).

CREATE DATABASE keycloak OWNER orpheus;
