-- 0032_settings_prov: the provenance of a project's settings (design
-- §13.1). JSON {"hash": <sha256 of the last imported project.yaml>,
-- "imported": {<key>: <the value that import set>}}. A key in settings
-- that the imported map does not hold with the same value is a local
-- override; a key in neither is the built-in default. '' = never imported.
ALTER TABLE projects ADD COLUMN settings_prov TEXT NOT NULL DEFAULT '';
