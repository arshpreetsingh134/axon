SET pgaudit.log = 'none';
CREATE EXTENSION IF NOT EXISTS pgaudit;
RESET pgaudit.log;
DO $mycel_audit$
DECLARE classes text[] := string_to_array(lower(replace(current_setting('pgaudit.log'), ' ', '')), ',');
BEGIN
  IF NOT (ARRAY['write','ddl','role'] <@ classes OR 'all' = ANY(classes))
     OR current_setting('pgaudit.log_parameter') <> 'off'
     OR current_setting('pgaudit.log_statement') <> 'off'
     OR current_setting('pgaudit.log_client') <> 'off'
     OR current_setting('log_statement') <> 'none' THEN
    RAISE EXCEPTION 'Managed PostgreSQL audit policy is not ready; reconcile the reviewed infrastructure through Mycel';
  END IF;
END
$mycel_audit$;
