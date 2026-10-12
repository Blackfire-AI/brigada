-- 0001_init: the initial schema of the engine store (design §11, with the
-- job and lease fields of §12.3, §14 and §6.4).
--
-- Conventions:
--   * Timestamps are UTC text in one fixed format, so lexicographic order
--     is chronological order.
--   * Booleans are integers constrained to 0 and 1.
--   * Columns holding JSON (settings, caps, payloads) are text; the
--     engine owns their inner shape.
--
-- Deletion: operational rows cascade to what they own (a project to its
-- plans and tokens, a plan to its tasks, a task to its jobs, a job to its
-- artifacts, a runner to its agents). The event log, being append-only,
-- references its referents without an ON DELETE action, so anything the
-- log already names cannot be deleted: the engine cancels state, it does
-- not erase history (§9).

-- One row per repository: the engine's unit of work and the foreman's
-- scope. One foreman per project (§4.1), hence the unique repo URL.
CREATE TABLE projects (
    id               INTEGER PRIMARY KEY,
    repo_url         TEXT NOT NULL UNIQUE,       -- repos are addressed by URL (product §5)
    default_branch   TEXT NOT NULL,              -- worktrees and result branches build on it
    settings         TEXT NOT NULL DEFAULT '{}', -- JSON: approval limit, retention, budgets (§6.3, §4.3)
    foreman_harness  TEXT NOT NULL,              -- the CLI the foreman runs on; must resume headless (§4.1)
    foreman_model    TEXT NOT NULL,              -- the model the foreman runs on
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- A plan is one DAG of tasks for a project (§6.1), built by the foreman
-- and mirrored into the repo as a plan file (§6.4).
CREATE TABLE plans (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('draft', 'pending_approval', 'active', 'done', 'cancelled')), -- §6.2
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- One task of a plan (§6.1). class carries no CHECK on purpose: the
-- launch set is implement, review and answer, and §6.3 explicitly allows
-- other classes (they merely need approval), so the set stays open.
CREATE TABLE tasks (
    id          INTEGER PRIMARY KEY,
    plan_id     INTEGER NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    spec        TEXT NOT NULL,              -- handed to the worker verbatim (§1.4)
    check_cmd   TEXT,                       -- the command the runner runs after a done (§5.5); NULL = none
    class       TEXT NOT NULL,
    state       TEXT NOT NULL CHECK (state IN ('pending', 'ready', 'running', 'checking', 'reviewing', 'done', 'needs_foreman', 'needs_user', 'failed', 'cancelled')), -- §6.1
    repo_url    TEXT,                       -- NULL = the project's repo; a task may name its own (§6.1)
    base_ref    TEXT NOT NULL,              -- the ref the worktree starts from
    branch      TEXT,                       -- the result branch the worker pushes (§6.4)
    caps        TEXT NOT NULL DEFAULT '{}', -- JSON: caps on fix, review and restart rounds (§14)
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- The plan's edges: task_id depends on dep_id (§6.1). The primary key
-- refuses duplicates and the CHECK refuses self-edges; acyclicity is the
-- engine's to check.
CREATE TABLE task_deps (
    task_id  INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    dep_id   INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, dep_id),
    CHECK (task_id <> dep_id)
);

-- One unit of dispatchable work (§12.3, §14). Worker jobs (work, fix,
-- review) belong to a task; a foreman turn (§4.2) belongs to a project
-- alone, hence the nullable task_id and the project reference.
-- status: queued, leased and running are live (see the partial index
-- below); done, failed, cancelled and stalled are terminal — the runner
-- completes a stalled job as such (§5.6). The worker's done, needs_help
-- and failed (§5.4) live in result, not here: they are what the job
-- ended saying, not whether it ended.
CREATE TABLE jobs (
    id                INTEGER PRIMARY KEY,
    project_id        INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id           INTEGER REFERENCES tasks(id) ON DELETE CASCADE,          -- NULL for kind = 'foreman'
    kind              TEXT NOT NULL CHECK (kind IN ('work', 'fix', 'review', 'foreman')),
    key               TEXT NOT NULL,                                           -- 'task:<id>' or 'foreman:<project>' (§14); at most one live job per key
    agent_id          INTEGER REFERENCES agents(id) ON DELETE SET NULL,        -- the agent dispatched to; NULL until then
    mode              TEXT NOT NULL CHECK (mode IN ('fresh', 'session')),
    prompt            TEXT NOT NULL,                                           -- the composed prompt: template plus verbatim data (§1.4)
    status            TEXT NOT NULL CHECK (status IN ('queued', 'leased', 'running', 'done', 'failed', 'cancelled', 'stalled')),
    lease_id          TEXT,                                                    -- set while a runner holds the job (§12.3)
    lease_owner       INTEGER REFERENCES runners(id) ON DELETE SET NULL,       -- the runner holding the lease
    lease_expires_at  TEXT,                                                    -- expiry is what makes a crashed runner safe (§5.1)
    fence_token       INTEGER,                                                 -- bumped per lease; a complete must echo the current one (§5.1, §6.4)
    followup          INTEGER NOT NULL DEFAULT 0 CHECK (followup IN (0, 1)),   -- a duplicate creation coalesced into this job (§12.1)
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),        -- leases taken; routine retries increment it (§3)
    result            TEXT,                                                    -- JSON: the worker's result block (§5.4)
    check_result      TEXT,                                                    -- JSON: exit code and output tail of the runner's check (§5.5)
    last_progress_at  TEXT,                                                    -- the last progress event (§5.6)
    last_activity_at  TEXT,                                                    -- the last sign of life; stalls are measured from it (§5.6)
    transcript_id     INTEGER REFERENCES artifacts(id) ON DELETE SET NULL,     -- the session transcript (§9)
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

-- Artifacts hold what must not be stuffed into the event log: full
-- session transcripts after each job and each foreman turn, and check
-- logs (§9, §5.5).
CREATE TABLE artifacts (
    id          INTEGER PRIMARY KEY,
    job_id      INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('transcript', 'check_log')),
    path        TEXT NOT NULL, -- path or blob ref in the artifact store
    size        INTEGER NOT NULL CHECK (size >= 0),
    sha256      TEXT NOT NULL, -- hex digest of the content
    created_at  TEXT NOT NULL
);

-- A resumable session: the foreman's 'foreman:<project>' (§4.1) and the
-- workers' per-task sessions. No state lives only in a session (§1.3);
-- this row is what lets a new process resume one.
CREATE TABLE sessions (
    id                  INTEGER PRIMARY KEY,
    key                 TEXT NOT NULL UNIQUE,
    agent_id            INTEGER REFERENCES agents(id) ON DELETE SET NULL, -- affinity: a resumed session stays on its agent (§8)
    runner_id           INTEGER REFERENCES runners(id) ON DELETE SET NULL,
    harness_session_id  TEXT, -- the CLI's own session id, written back on complete (§4.2)
    state               TEXT NOT NULL CHECK (state IN ('active', 'ended')),
    last_used_at        TEXT,
    created_at          TEXT NOT NULL
);

-- A runner process, registered with a runner token (§5.1). The adapters
-- it advertises become its agents.
CREATE TABLE runners (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT
);

-- One advertised (harness, model, account) combination on a runner
-- (§5.1): what the dispatcher chooses between (§8). classes is JSON for
-- the same reason tasks.class carries no CHECK.
CREATE TABLE agents (
    id            INTEGER PRIMARY KEY,
    runner_id     INTEGER NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
    adapter       TEXT NOT NULL, -- one adapter per CLI (§5.2)
    harness       TEXT NOT NULL,
    model         TEXT NOT NULL,
    family        TEXT NOT NULL, -- model family; a reviewer's must differ from the author's (§7)
    account       TEXT NOT NULL, -- account label; quota cooling covers the whole account (§8)
    classes       TEXT NOT NULL, -- JSON array of task classes it can take
    health        TEXT NOT NULL DEFAULT 'ready' CHECK (health IN ('ready', 'broken')), -- §8
    cooling_until TEXT,           -- while in the future, not eligible (§8)
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- The dispatcher's memory: counts per (repo, class, agent), from which
-- the success rate with its prior is derived (§8). Success = check
-- passed and review approved within the rounds.
CREATE TABLE outcomes (
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    agent_id    INTEGER NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    class       TEXT NOT NULL, -- tasks' open class set (§6.1)
    attempts    INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    successes   INTEGER NOT NULL DEFAULT 0 CHECK (successes >= 0),
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (project_id, agent_id, class)
);

-- The append-only event log, the backbone the history hangs off (§9).
-- kind is deliberately not an enum: new event kinds arrive with engine
-- features. The references carry no ON DELETE action (see the file
-- header): what the log names cannot be deleted.
CREATE TABLE events (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,
    project_id  INTEGER REFERENCES projects(id),
    task_id     INTEGER REFERENCES tasks(id),
    job_id      INTEGER REFERENCES jobs(id),
    payload     TEXT NOT NULL DEFAULT '{}', -- JSON: the event's data
    created_at  TEXT NOT NULL
);

-- The log is append-only (§9): no row may change once written.
CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only: UPDATE refused');
END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only: DELETE refused');
END;

-- The conversation history, one timeline per project (§9): chat turns,
-- tool calls and results, task specs and results, questions and answers,
-- reviews, notifications. kind is presentation-level and open, like
-- events.kind.
CREATE TABLE messages (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    event_id    INTEGER REFERENCES events(id), -- the log entry behind the message, when there is one (§9)
    kind        TEXT NOT NULL,
    author      TEXT NOT NULL CHECK (author IN ('user', 'foreman', 'worker', 'engine')),
    content     TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

-- A question waiting for a person: a worker's needs_help (§5.4) or the
-- foreman's ask_user (§12.2). An answer closes it (§12.2 answer).
CREATE TABLE questions (
    id           INTEGER PRIMARY KEY,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id      INTEGER REFERENCES tasks(id) ON DELETE CASCADE, -- the asking worker's task, when there is one
    question     TEXT NOT NULL,
    answer       TEXT,
    answered_at  TEXT,
    created_at   TEXT NOT NULL
);

-- Bearer tokens by role (§12.1). Only the sha256 of the secret is
-- stored, never the secret itself; the secrets live in a config file
-- outside the store (§6.4).
CREATE TABLE tokens (
    id          INTEGER PRIMARY KEY,
    role        TEXT NOT NULL CHECK (role IN ('client', 'foreman', 'runner', 'admin')),
    token_hash  TEXT NOT NULL UNIQUE, -- sha256 hex of the secret
    project_id  INTEGER REFERENCES projects(id) ON DELETE CASCADE, -- foreman tokens are scoped to one project (§12.1)
    created_at  TEXT NOT NULL
);

-- Store-wide state that belongs to no table above. The single row is
-- created here and never deleted.
CREATE TABLE meta (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    fence_epoch  INTEGER NOT NULL DEFAULT 1 CHECK (fence_epoch >= 1) -- bumped by recover; older-epoch leases are refused (§6.4)
);
INSERT INTO meta (id, fence_epoch) VALUES (1, 1);

-- The obvious lookups.

-- At most one live job per key (§14). The live set is queued, leased and
-- running: a job waiting for a runner, held by one, or executing. The
-- terminal statuses (done, failed, cancelled, stalled) leave the key
-- free, so a finished job plus a new live one is fine.
CREATE UNIQUE INDEX idx_jobs_live_key ON jobs(key) WHERE status IN ('queued', 'leased', 'running');

-- A lease id names one lease; NULL (no lease) must not collide.
CREATE UNIQUE INDEX idx_jobs_lease_id ON jobs(lease_id) WHERE lease_id IS NOT NULL;

CREATE INDEX idx_jobs_status ON jobs(status);
CREATE INDEX idx_jobs_key ON jobs(key);
CREATE INDEX idx_jobs_lease_expires_at ON jobs(lease_expires_at);
CREATE INDEX idx_jobs_task ON jobs(task_id);
CREATE INDEX idx_tasks_plan ON tasks(plan_id);
CREATE INDEX idx_events_time ON events(created_at);
CREATE INDEX idx_events_project ON events(project_id);
CREATE INDEX idx_messages_project_time ON messages(project_id, created_at);
CREATE INDEX idx_artifacts_job ON artifacts(job_id);
