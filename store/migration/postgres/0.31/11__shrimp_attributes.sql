-- Retain exact owned scalar facts independently of native account fields.
ALTER TABLE shrimp_subject ADD COLUMN attributes TEXT NOT NULL DEFAULT 'null';
UPDATE shrimp_subject SET attributes=json_build_object('displayName', json_build_object('value',display_name,'authority','','revision',revision))::text;
