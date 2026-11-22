-- Replace direct privileges on this catalog, including older column grants.
REVOKE ALL PRIVILEGES ON SCHEMA {{schema}} FROM {{role}};
REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA {{schema}} FROM {{role}};
REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA {{schema}} FROM {{role}};
REVOKE ALL PRIVILEGES ON ALL ROUTINES IN SCHEMA {{schema}} FROM {{role}};

GRANT USAGE ON SCHEMA {{schema}} TO {{role}};
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA {{schema}} TO {{role}};
GRANT EXECUTE ON ALL ROUTINES IN SCHEMA {{schema}} TO {{role}};

-- Setup and restore own the catalog identity and version stamp.
REVOKE INSERT, UPDATE, DELETE ON {{schema}}.vault_metadata FROM {{role}};
