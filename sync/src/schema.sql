CREATE TABLE IF NOT EXISTS launches (
    ordinal INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL CHECK(provider IN ('codex', 'claude')),
    instance TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS members (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    instance TEXT NOT NULL,
    session TEXT NOT NULL,
    agent TEXT NOT NULL,
    parent TEXT REFERENCES members(id),
    launch TEXT NOT NULL REFERENCES launches(id),
    ordinal INTEGER NOT NULL,
    UNIQUE(provider, instance, session, agent)
);
CREATE TABLE IF NOT EXISTS mailboxes (
    member TEXT PRIMARY KEY REFERENCES members(id),
    sent INTEGER NOT NULL DEFAULT 0,
    acked INTEGER NOT NULL DEFAULT 0,
    batch_end INTEGER NOT NULL DEFAULT 0,
    CHECK(acked <= sent),
    CHECK(batch_end = 0 OR (batch_end > acked AND batch_end <= sent))
);
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    recipient TEXT NOT NULL REFERENCES members(id),
    sequence INTEGER NOT NULL,
    sender TEXT NOT NULL REFERENCES members(id),
    body TEXT NOT NULL,
    UNIQUE(recipient, sequence)
);
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    creator TEXT NOT NULL REFERENCES members(id),
    owner TEXT NOT NULL REFERENCES members(id),
    description TEXT NOT NULL,
    result TEXT
);
CREATE TRIGGER IF NOT EXISTS immutable_result
BEFORE UPDATE OF result ON tasks WHEN OLD.result IS NOT NULL
BEGIN SELECT RAISE(ABORT, 'terminal task result is immutable'); END;
CREATE TABLE IF NOT EXISTS cells (
    key TEXT PRIMARY KEY,
    revision INTEGER NOT NULL CHECK(revision > 0),
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    request TEXT NOT NULL,
    response TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS stop_delivery (
    member TEXT NOT NULL REFERENCES members(id),
    launch TEXT NOT NULL REFERENCES launches(id),
    boundary TEXT NOT NULL,
    PRIMARY KEY(member, launch, boundary)
);
PRAGMA user_version = 1;
