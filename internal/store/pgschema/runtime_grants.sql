GRANT USAGE ON SCHEMA {{schema}} TO {{role}};
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA {{schema}} TO {{role}};
GRANT EXECUTE ON ALL ROUTINES IN SCHEMA {{schema}} TO {{role}};

-- Setup and restore own the catalog identity and version stamp.
REVOKE INSERT, UPDATE, DELETE ON {{schema}}.vault_metadata FROM {{role}};
