-- 0031_project_transport: a project's repo_url is its canonical identity
-- (host[:port]/path or file:<path>, design §13.1), which is no Git URL.
-- transport_url is the credential-free URL runners fetch with when a task
-- names none of its own; '' for a row startup has not rewritten yet, whose
-- repo_url is still the URL as it was given and serves both purposes.
ALTER TABLE projects ADD COLUMN transport_url TEXT NOT NULL DEFAULT '';
