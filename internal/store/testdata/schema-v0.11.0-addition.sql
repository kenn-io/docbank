-- v0.10.1 and v0.11.0 added this logical query index without changing
-- storage schema version 2 or physical authority.
CREATE INDEX audit_record_event_scope
    ON audit_records(scope_id) WHERE kind = 'event';
