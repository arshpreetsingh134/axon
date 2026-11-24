\connect :audit_db
BEGIN;
SET LOCAL pgaudit.log_client = on;
SET LOCAL pgaudit.log_level = notice;
SET LOCAL pgaudit.log_relation = on;
CREATE TEMP TABLE mycel_audit_probe (value integer);
INSERT INTO mycel_audit_probe VALUES (1);
ROLLBACK;
