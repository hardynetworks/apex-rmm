-- Apex RMM schema (applied idempotently at startup)

CREATE TABLE IF NOT EXISTS schema_version (version int PRIMARY KEY);

CREATE TABLE IF NOT EXISTS users (
    id            text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    subject       text UNIQUE,                 -- OIDC "sub" (NULL for local accounts)
    email         text NOT NULL DEFAULT '',
    name          text NOT NULL DEFAULT '',
    username      text NOT NULL DEFAULT '',
    role          text NOT NULL DEFAULT 'viewer', -- admin | technician | viewer
    groups        text[] NOT NULL DEFAULT '{}',
    password_hash text,                        -- local break-glass account only
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login    timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS users_local_email ON users (lower(email)) WHERE subject IS NULL;

CREATE TABLE IF NOT EXISTS sessions (
    id          text PRIMARY KEY,
    user_id     text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    id_token    text NOT NULL DEFAULT '',
    ip          text NOT NULL DEFAULT '',
    user_agent  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS clients (
    id         text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name       text NOT NULL UNIQUE,
    notes      text NOT NULL DEFAULT '',
    contact_name  text NOT NULL DEFAULT '',
    contact_email text NOT NULL DEFAULT '',
    contact_phone text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sites (
    id         text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    client_id  text NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (client_id, name)
);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    id          text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    token       text NOT NULL UNIQUE,
    client_id   text NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    site_id     text REFERENCES sites(id) ON DELETE SET NULL,
    description text NOT NULL DEFAULT '',
    expires_at  timestamptz,
    max_uses    int NOT NULL DEFAULT 0,  -- 0 = unlimited
    uses        int NOT NULL DEFAULT 0,
    revoked     boolean NOT NULL DEFAULT false,
    created_by  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS devices (
    id              text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    client_id       text REFERENCES clients(id) ON DELETE SET NULL,
    site_id         text REFERENCES sites(id) ON DELETE SET NULL,
    secret_hash     text NOT NULL,
    hostname        text NOT NULL DEFAULT '',
    display_name    text NOT NULL DEFAULT '',
    os              text NOT NULL DEFAULT '',
    platform        text NOT NULL DEFAULT '',
    os_version      text NOT NULL DEFAULT '',
    kernel_version  text NOT NULL DEFAULT '',
    arch            text NOT NULL DEFAULT '',
    cpu_model       text NOT NULL DEFAULT '',
    cpu_cores       int NOT NULL DEFAULT 0,
    ram_total       bigint NOT NULL DEFAULT 0,
    manufacturer    text NOT NULL DEFAULT '',
    model           text NOT NULL DEFAULT '',
    serial          text NOT NULL DEFAULT '',
    agent_version   text NOT NULL DEFAULT '',
    public_ip       text NOT NULL DEFAULT '',
    local_ips       text[] NOT NULL DEFAULT '{}',
    logged_in_users text[] NOT NULL DEFAULT '{}',
    inventory       jsonb NOT NULL DEFAULT '{}',
    online          boolean NOT NULL DEFAULT false,
    last_seen       timestamptz,
    boot_time       timestamptz,
    cpu_pct         real NOT NULL DEFAULT 0,
    mem_pct         real NOT NULL DEFAULT 0,
    disk_pct        real NOT NULL DEFAULT 0,
    rustdesk_id       text NOT NULL DEFAULT '',
    rustdesk_password text NOT NULL DEFAULT '',
    notes           text NOT NULL DEFAULT '',
    tags            text[] NOT NULL DEFAULT '{}',
    maintenance_until timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS devices_client ON devices (client_id);

CREATE TABLE IF NOT EXISTS device_metrics (
    device_id text NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    ts        timestamptz NOT NULL DEFAULT now(),
    cpu       real NOT NULL,
    mem       real NOT NULL,
    disk      real NOT NULL,
    net_rx    bigint NOT NULL DEFAULT 0,
    net_tx    bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS device_metrics_dev_ts ON device_metrics (device_id, ts DESC);

CREATE TABLE IF NOT EXISTS alert_policies (
    id               text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name             text NOT NULL,
    enabled          boolean NOT NULL DEFAULT true,
    metric           text NOT NULL,        -- cpu | mem | disk | offline
    operator         text NOT NULL DEFAULT '>',
    threshold        real NOT NULL DEFAULT 90,
    duration_minutes int NOT NULL DEFAULT 5,
    severity         text NOT NULL DEFAULT 'warning', -- info | warning | critical
    client_id        text REFERENCES clients(id) ON DELETE CASCADE, -- NULL = all clients
    create_ticket    boolean NOT NULL DEFAULT false,
    notify           boolean NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS alerts (
    id              text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    policy_id       text REFERENCES alert_policies(id) ON DELETE SET NULL,
    device_id       text REFERENCES devices(id) ON DELETE CASCADE,
    severity        text NOT NULL DEFAULT 'warning',
    title           text NOT NULL,
    message         text NOT NULL DEFAULT '',
    value           real,
    status          text NOT NULL DEFAULT 'open', -- open | acknowledged | resolved
    ticket_id       text,
    acknowledged_by text,
    acknowledged_at timestamptz,
    resolved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS alerts_status ON alerts (status, created_at DESC);

CREATE TABLE IF NOT EXISTS scripts (
    id              text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name            text NOT NULL,
    description     text NOT NULL DEFAULT '',
    category        text NOT NULL DEFAULT 'General',
    shell           text NOT NULL,          -- powershell | pwsh | cmd | bash | sh | zsh | python
    platforms       text[] NOT NULL DEFAULT '{}',
    body            text NOT NULL,
    timeout_seconds int NOT NULL DEFAULT 300,
    created_by      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS jobs (
    id          text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name        text NOT NULL,
    script_id   text REFERENCES scripts(id) ON DELETE SET NULL,
    schedule_id text,
    shell       text NOT NULL,
    body        text NOT NULL,
    args        text[] NOT NULL DEFAULT '{}',
    timeout_seconds int NOT NULL DEFAULT 300,
    source      text NOT NULL DEFAULT 'manual',
    created_by  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS job_results (
    id          text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    job_id      text NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    device_id   text NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    status      text NOT NULL DEFAULT 'pending', -- pending | running | success | failed | timeout | error | expired
    exit_code   int,
    stdout      text NOT NULL DEFAULT '',
    stderr      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz
);
CREATE INDEX IF NOT EXISTS job_results_job ON job_results (job_id);
CREATE INDEX IF NOT EXISTS job_results_dev ON job_results (device_id, created_at DESC);
CREATE INDEX IF NOT EXISTS job_results_pending ON job_results (device_id) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS schedules (
    id          text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name        text NOT NULL,
    script_id   text NOT NULL REFERENCES scripts(id) ON DELETE CASCADE,
    cron        text NOT NULL,
    target_type text NOT NULL DEFAULT 'devices', -- devices | client | all
    target_ids  text[] NOT NULL DEFAULT '{}',
    enabled     boolean NOT NULL DEFAULT true,
    last_run    timestamptz,
    next_run    timestamptz,
    created_by  text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE SEQUENCE IF NOT EXISTS ticket_number_seq START 1001;

CREATE TABLE IF NOT EXISTS tickets (
    id              text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    number          bigint NOT NULL DEFAULT nextval('ticket_number_seq') UNIQUE,
    title           text NOT NULL,
    description     text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'open',   -- open | in_progress | waiting | resolved | closed
    priority        text NOT NULL DEFAULT 'medium', -- low | medium | high | urgent
    category        text NOT NULL DEFAULT '',
    client_id       text REFERENCES clients(id) ON DELETE SET NULL,
    device_id       text REFERENCES devices(id) ON DELETE SET NULL,
    assignee_id     text REFERENCES users(id) ON DELETE SET NULL,
    requester_name  text NOT NULL DEFAULT '',
    requester_email text NOT NULL DEFAULT '',
    source          text NOT NULL DEFAULT 'manual', -- manual | alert
    due_at          timestamptz,
    created_by      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    resolved_at     timestamptz
);
CREATE INDEX IF NOT EXISTS tickets_status ON tickets (status, updated_at DESC);

CREATE TABLE IF NOT EXISTS ticket_comments (
    id           text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    ticket_id    text NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    author_id    text REFERENCES users(id) ON DELETE SET NULL,
    author_name  text NOT NULL DEFAULT '',
    body         text NOT NULL,
    internal     boolean NOT NULL DEFAULT false,
    time_minutes int NOT NULL DEFAULT 0,
    kind         text NOT NULL DEFAULT 'comment', -- comment | event
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ticket_comments_ticket ON ticket_comments (ticket_id, created_at);

CREATE TABLE IF NOT EXISTS audit_log (
    id          bigserial PRIMARY KEY,
    user_id     text,
    user_name   text NOT NULL DEFAULT '',
    action      text NOT NULL,
    target_type text NOT NULL DEFAULT '',
    target_id   text NOT NULL DEFAULT '',
    details     jsonb NOT NULL DEFAULT '{}',
    ip          text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_log_created ON audit_log (created_at DESC);

CREATE TABLE IF NOT EXISTS settings (
    key   text PRIMARY KEY,
    value jsonb NOT NULL
);
