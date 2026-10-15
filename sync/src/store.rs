use crate::{barrier_point, crash_point, types::*, wake};
use anyhow::{Context, Result, bail, ensure};
use rusqlite::{Connection, OptionalExtension, TransactionBehavior, params};
use serde_json::{Value, json};
use std::{
    fs,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    time::{Duration, Instant},
};

pub struct Store {
    pub conn: Connection,
    pub directory: PathBuf,
}

fn open_database(path: &Path) -> Result<Connection> {
    // SQLite 3.51.3 fixes the WAL-reset race with concurrent writers and
    // checkpoints. The bundled dependency must not regress below that fix.
    ensure!(
        rusqlite::version_number() >= 3_051_003,
        "SQLite 3.51.3+ required"
    );
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        let attempt = (|| -> Result<Connection> {
            let conn = Connection::open(path)?;
            // Each SQLite lock acquisition has its own busy timeout. Let the
            // outer retry own the shared bootstrap contention budget instead.
            conn.busy_timeout(Duration::ZERO)?;
            conn.pragma_update(None, "journal_mode", "WAL")?;
            conn.pragma_update(None, "synchronous", "FULL")?;
            conn.pragma_update(None, "fullfsync", "ON")?;
            conn.pragma_update(None, "checkpoint_fullfsync", "ON")?;
            conn.pragma_update(None, "foreign_keys", "ON")?;
            let version: i64 = conn.pragma_query_value(None, "user_version", |r| r.get(0))?;
            ensure!(
                version <= 1,
                "database schema {version} is newer than this helper"
            );
            if version == 0 {
                conn.execute_batch(&format!(
                    "BEGIN IMMEDIATE; {} COMMIT;",
                    include_str!("schema.sql")
                ))?;
            }
            conn.busy_timeout(Duration::from_secs(5))?;
            Ok(conn)
        })();
        match attempt {
            Ok(conn) => return Ok(conn),
            Err(error) => {
                let busy = matches!(
                    error.downcast_ref::<rusqlite::Error>(),
                    Some(rusqlite::Error::SqliteFailure(code, _))
                        if matches!(code.code, rusqlite::ErrorCode::DatabaseBusy
                            | rusqlite::ErrorCode::DatabaseLocked)
                );
                if !busy || Instant::now() >= deadline {
                    return Err(error).context("opening coordination database");
                }
                // WAL conversion may return BUSY without invoking SQLite's
                // busy handler. Drop the failed connection/transaction and
                // retry bootstrap, never a user operation, within one budget.
                std::thread::sleep(
                    Duration::from_millis(5)
                        .min(deadline.saturating_duration_since(Instant::now())),
                );
            }
        }
    }
}

impl Store {
    pub fn open(home: &Path, group: &GroupId) -> Result<Self> {
        ensure!(
            group.as_str() != "." && group.as_str() != "..",
            "invalid group"
        );
        let directory = home.join("groups").join(group.as_str());
        fs::create_dir_all(&directory)?;
        fs::set_permissions(&directory, fs::Permissions::from_mode(0o700))?;
        barrier_point("before_open")?;
        let conn = open_database(&directory.join("coordination.db"))?;
        fs::set_permissions(
            directory.join("coordination.db"),
            fs::Permissions::from_mode(0o600),
        )?;
        Ok(Self { conn, directory })
    }

    pub fn execute(&mut self, request: &Request) -> Result<Value> {
        self.execute_timed(request).map(|(result, _)| result)
    }

    /// The optional timestamp is taken immediately after COMMIT returns. It
    /// is process-local measurement data, never coordination state.
    pub fn execute_timed(&mut self, request: &Request) -> Result<(Value, Option<Instant>)> {
        ensure!(request.version == 1, "unsupported protocol version");
        ensure!(
            !matches!(request.command, Command::WaitAny { .. }),
            "wait requires wait_any dispatcher"
        );
        if !request.command.is_mutation() {
            let tx = self.conn.transaction()?;
            authenticate(&tx, request)?;
            let result = read(&tx, request)?;
            tx.commit()?;
            return Ok((result, None));
        }
        let operation = request
            .operation_id
            .as_ref()
            .context("operation_id required for mutation")?;
        let encoded = serde_json::to_string(request)?;
        ensure!(encoded.len() <= 65536, "request exceeds 64 KiB");
        let tx = self
            .conn
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        // Authenticate before replay: a superseded helper cannot revive its
        // identity by replaying a previously successful operation.
        if !matches!(
            request.command,
            Command::LaunchStart { .. } | Command::Bind { .. }
        ) {
            authenticate(&tx, request)?;
        }
        let old: Option<(String, String)> = tx
            .query_row(
                "SELECT request, response FROM operations WHERE id = ?1",
                [operation.as_str()],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .optional()?;
        if let Some((args, result)) = old {
            ensure!(
                args == encoded,
                "operation_id reused with different arguments"
            );
            return Ok((serde_json::from_str(&result)?, None));
        }
        let result = mutate(&tx, request)?;
        tx.execute(
            "INSERT INTO operations(id, request, response) VALUES (?1, ?2, ?3)",
            params![operation.as_str(), encoded, serde_json::to_string(&result)?],
        )?;
        barrier_point("before_commit")?;
        crash_point("before_commit");
        tx.commit()?;
        let committed = Instant::now();
        barrier_point("after_commit")?;
        crash_point("after_commit");
        wake::notify(&self.directory);
        crash_point("after_notify");
        Ok((result, Some(committed)))
    }

    pub fn ready(&mut self, request: &Request) -> Result<Value> {
        let Command::WaitAny {
            messages,
            tasks,
            cells,
            ..
        } = &request.command
        else {
            bail!("expected wait_any");
        };
        let tx = self.conn.transaction()?;
        let member = authenticate(&tx, request)?;
        let pending: bool = tx.query_row(
            "SELECT sent > acked FROM mailboxes WHERE member = ?1",
            [member.as_str()],
            |r| r.get(0),
        )?;
        let mut finished = Vec::new();
        for task in tasks {
            let value = task_read(&tx, task)?;
            if value["result"] != Value::Null {
                finished.push(value);
            }
        }
        let mut changed = Vec::new();
        for watch in cells {
            let value = cell_read(&tx, &watch.key)?;
            if value["revision"].as_u64() != Some(watch.revision.0) {
                changed.push(value);
            }
        }
        // End this snapshot before sleeping. The next poll opens a fresh one.
        tx.commit()?;
        Ok(json!({"messages": *messages && pending, "tasks": finished, "cells": changed}))
    }

    pub fn history(&self) -> Result<Value> {
        let mut stmt = self
            .conn
            .prepare("SELECT sequence, id, request, response FROM operations ORDER BY sequence")?;
        let rows = stmt.query_map([], |r| {
            Ok((
                r.get::<_, i64>(0)?,
                r.get::<_, String>(1)?,
                r.get::<_, String>(2)?,
                r.get::<_, String>(3)?,
            ))
        })?;
        let mut result = Vec::new();
        for row in rows {
            let (sequence, id, request, response) = row?;
            result.push(json!({"sequence":sequence,"operation_id":id,
                "request":serde_json::from_str::<Value>(&request)?,
                "result":serde_json::from_str::<Value>(&response)?}));
        }
        Ok(json!(result))
    }

    pub fn claim_stop(&mut self, request: &Request, boundary: &str) -> Result<bool> {
        let tx = self
            .conn
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let member = authenticate(&tx, request)?;
        let launch = request.launch.as_ref().context("launch required")?;
        let inserted = tx.execute(
            "INSERT OR IGNORE INTO stop_delivery VALUES (?1, ?2, ?3)",
            params![member.as_str(), launch.as_str(), boundary],
        )? == 1;
        tx.commit()?;
        Ok(inserted)
    }

    pub fn lookup(
        &self,
        launch: &LaunchId,
        session: &str,
        agent: Option<&str>,
    ) -> Result<MemberId> {
        let id: String = self
            .conn
            .query_row(
                "SELECT m.id FROM members m JOIN launches l ON l.id = ?1
             WHERE m.provider = l.provider AND m.instance = l.instance
               AND m.session = ?2 AND m.agent = ?3 AND m.launch = l.id",
                params![launch.as_str(), session, agent.unwrap_or("")],
                |r| r.get(0),
            )
            .context("native identity was not registered by its startup hook")?;
        MemberId::try_from(id)
    }
}

fn authenticate<'a>(conn: &Connection, req: &'a Request) -> Result<&'a MemberId> {
    let member = req.member.as_ref().context("explicit member required")?;
    let launch = req.launch.as_ref().context("explicit launch required")?;
    let valid: bool = conn.query_row(
        "WITH RECURSIVE lineage(id, parent, launch) AS (
           SELECT id, parent, launch FROM members WHERE id = ?1
           UNION ALL SELECT m.id, m.parent, m.launch FROM members m
           JOIN lineage l ON m.id = l.parent
         ) SELECT EXISTS(SELECT 1 FROM lineage)
           AND NOT EXISTS(SELECT 1 FROM lineage WHERE launch != ?2)",
        params![member.as_str(), launch.as_str()],
        |r| r.get(0),
    )?;
    ensure!(valid, "unknown member or stale launch");
    Ok(member)
}

fn mutate(conn: &Connection, req: &Request) -> Result<Value> {
    match &req.command {
        Command::LaunchStart { provider, instance } => {
            bounded_text(instance, 128, "instance")?;
            let launch = LaunchId::new();
            conn.execute(
                "INSERT INTO launches(id, provider, instance) VALUES (?1, ?2, ?3)",
                params![launch.as_str(), provider.as_str(), instance],
            )?;
            Ok(json!({"launch": launch}))
        }
        Command::Bind {
            session,
            agent,
            parent_agent,
        } => bind(
            conn,
            req,
            session,
            agent.as_deref(),
            parent_agent.as_deref(),
        ),
        Command::Send { to, body } => {
            bounded_text(body, 2048, "message body")?;
            let member = req.member.as_ref().unwrap();
            let sequence: i64 = conn
                .query_row(
                    "UPDATE mailboxes SET sent = sent + 1 WHERE member = ?1 RETURNING sent",
                    [to.as_str()],
                    |r| r.get(0),
                )
                .context("unknown recipient")?;
            let message = MessageId::new();
            conn.execute(
                "INSERT INTO messages VALUES (?1, ?2, ?3, ?4, ?5)",
                params![
                    message.as_str(),
                    to.as_str(),
                    sequence,
                    member.as_str(),
                    body
                ],
            )?;
            Ok(json!({"message": message, "sequence": sequence}))
        }
        Command::Inbox { limit } => inbox(conn, req.member.as_ref().unwrap(), limit.unwrap_or(4)),
        Command::Ack { through } => {
            let member = req.member.as_ref().unwrap();
            let sequence: i64 = conn
                .query_row(
                    "SELECT sequence FROM messages WHERE id = ?1 AND recipient = ?2",
                    params![through.as_str(), member.as_str()],
                    |r| r.get(0),
                )
                .context("acknowledgement does not belong to this mailbox")?;
            let (acked, end): (i64, i64) = conn.query_row(
                "SELECT acked, batch_end FROM mailboxes WHERE member = ?1",
                [member.as_str()],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )?;
            if sequence > acked {
                ensure!(sequence <= end, "cannot acknowledge beyond fetched prefix");
                conn.execute("UPDATE mailboxes SET acked = ?2, batch_end = CASE WHEN ?2 = batch_end THEN 0 ELSE batch_end END WHERE member = ?1",
                    params![member.as_str(), sequence])?;
            }
            Ok(json!({"acked": sequence.max(acked)}))
        }
        Command::TaskCreate { owner, description } => {
            bounded_text(description, 4096, "description")?;
            let task = TaskId::new();
            conn.execute(
                "INSERT INTO tasks(id, creator, owner, description) VALUES (?1, ?2, ?3, ?4)",
                params![
                    task.as_str(),
                    req.member.as_ref().unwrap().as_str(),
                    owner.as_str(),
                    description
                ],
            )?;
            Ok(json!({"task":task}))
        }
        Command::TaskFinish { task, result } => {
            let value = serde_json::to_string(result)?;
            ensure!(value.len() <= 16384, "task result exceeds 16 KiB");
            let (owner, old): (String, Option<String>) = conn.query_row(
                "SELECT owner, result FROM tasks WHERE id = ?1",
                [task.as_str()],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )?;
            ensure!(
                owner == req.member.as_ref().unwrap().as_str(),
                "only task owner can finish"
            );
            if let Some(old) = old {
                ensure!(old == value, "terminal task result is immutable");
            } else {
                conn.execute(
                    "UPDATE tasks SET result = ?2 WHERE id = ?1",
                    params![task.as_str(), value],
                )?;
            }
            task_read(conn, task)
        }
        Command::CellCas {
            key,
            expected,
            value,
        } => {
            bounded_text(key, 128, "cell key")?;
            let encoded = serde_json::to_string(value)?;
            ensure!(encoded.len() <= 16384, "cell value exceeds 16 KiB");
            let current = cell_read(conn, key)?;
            if current["revision"].as_u64() != Some(expected.0) {
                return Ok(json!({"swapped":false,"cell":current}));
            }
            ensure!(expected.0 < i64::MAX as u64 - 1, "revision exhausted");
            conn.execute("INSERT INTO cells VALUES (?1, ?2, ?3) ON CONFLICT(key) DO UPDATE SET revision=excluded.revision, value=excluded.value",
                params![key, (expected.0+1) as i64, encoded])?;
            Ok(json!({"swapped":true,"cell": {"key":key,"revision":expected.0+1,"value":value}}))
        }
        _ => bail!("expected mutation"),
    }
}

fn bind(
    conn: &Connection,
    req: &Request,
    session: &str,
    agent: Option<&str>,
    parent_agent: Option<&str>,
) -> Result<Value> {
    bounded_text(session, 128, "session")?;
    if let Some(agent) = agent {
        bounded_text(agent, 128, "agent")?;
    }
    let launch = req.launch.as_ref().context("launch required")?;
    let (provider, instance, ordinal): (String, String, i64) = conn.query_row(
        "SELECT provider, instance, ordinal FROM launches WHERE id = ?1",
        [launch.as_str()],
        |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
    )?;
    let parent: Option<String> = if agent.is_some() {
        Some(conn.query_row("SELECT id FROM members WHERE provider=?1 AND instance=?2 AND session=?3 AND agent=?4 AND launch=?5",
            params![provider, instance, session, parent_agent.unwrap_or(""), launch.as_str()],
            |r| r.get(0)).context("parent startup hook has not registered in this launch")?)
    } else {
        ensure!(
            parent_agent.is_none(),
            "main agent cannot have parent_agent"
        );
        None
    };
    if let Some(parent_id) = &parent {
        let parent_req = Request {
            version: 1,
            operation_id: None,
            member: Some(MemberId::try_from(parent_id.clone())?),
            launch: Some(launch.clone()),
            command: Command::Members,
        };
        authenticate(conn, &parent_req)?;
    }
    let old: Option<(String,i64,Option<String>)> = conn.query_row(
        "SELECT id, ordinal, parent FROM members WHERE provider=?1 AND instance=?2 AND session=?3 AND agent=?4",
        params![provider,instance,session,agent.unwrap_or("")],
        |r| Ok((r.get(0)?,r.get(1)?,r.get(2)?))).optional()?;
    let member = if let Some((id, old_ordinal, old_parent)) = old {
        ensure!(ordinal >= old_ordinal, "stale startup hook");
        ensure!(
            old_parent == parent,
            "native identity cannot change parents"
        );
        conn.execute(
            "UPDATE members SET launch=?2, ordinal=?3 WHERE id=?1",
            params![id, launch.as_str(), ordinal],
        )?;
        MemberId::try_from(id)?
    } else {
        let member = MemberId::new();
        conn.execute(
            "INSERT INTO members VALUES (?1,?2,?3,?4,?5,?6,?7,?8)",
            params![
                member.as_str(),
                provider,
                instance,
                session,
                agent.unwrap_or(""),
                parent,
                launch.as_str(),
                ordinal
            ],
        )?;
        conn.execute(
            "INSERT INTO mailboxes(member) VALUES (?1)",
            [member.as_str()],
        )?;
        member
    };
    Ok(json!({"member":member,"launch":launch,"parent":parent}))
}

fn read(conn: &Connection, req: &Request) -> Result<Value> {
    match &req.command {
        Command::CellRead { key } => cell_read(conn, key),
        Command::TaskRead { task } => task_read(conn, task),
        Command::Members => {
            let mut stmt = conn.prepare("SELECT id, provider, instance, session, agent, parent, launch FROM members ORDER BY rowid")?;
            let rows = stmt.query_map([], |r| {
                Ok(json!({"member":r.get::<_,String>(0)?,
                "provider":r.get::<_,String>(1)?,"instance":r.get::<_,String>(2)?,
                "session":r.get::<_,String>(3)?,"agent":r.get::<_,String>(4)?,
                "parent":r.get::<_,Option<String>>(5)?,"launch":r.get::<_,String>(6)?}))
            })?;
            Ok(json!({"members": rows.collect::<rusqlite::Result<Vec<_>>>()?}))
        }
        _ => bail!("expected read"),
    }
}

fn inbox(conn: &Connection, member: &MemberId, limit: u32) -> Result<Value> {
    ensure!((1..=4).contains(&limit), "inbox limit must be 1..4");
    let (acked, mut end, sent): (i64, i64, i64) = conn.query_row(
        "SELECT acked, batch_end, sent FROM mailboxes WHERE member=?1",
        [member.as_str()],
        |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
    )?;
    if end == 0 && sent > acked {
        end = sent.min(acked + i64::from(limit));
        conn.execute(
            "UPDATE mailboxes SET batch_end=?2 WHERE member=?1",
            params![member.as_str(), end],
        )?;
    }
    let mut stmt = conn.prepare("SELECT id, sequence, sender, body FROM messages WHERE recipient=?1 AND sequence>?2 AND sequence<=?3 ORDER BY sequence LIMIT ?4")?;
    let rows = stmt.query_map(params![member.as_str(), acked, end, limit], |r| {
        Ok(json!({
        "message":r.get::<_,String>(0)?,"sequence":r.get::<_,i64>(1)?,
        "from":r.get::<_,String>(2)?,"body":r.get::<_,String>(3)?}))
    })?;
    Ok(
        json!({"messages":rows.collect::<rusqlite::Result<Vec<_>>>()?,"acked":acked,"batch_end":end}),
    )
}

fn cell_read(conn: &Connection, key: &str) -> Result<Value> {
    bounded_text(key, 128, "cell key")?;
    let row: Option<(i64, String)> = conn
        .query_row(
            "SELECT revision,value FROM cells WHERE key=?1",
            [key],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .optional()?;
    let (revision, value) = match row {
        Some((rev, text)) => (rev, serde_json::from_str::<Value>(&text)?),
        None => (0, Value::Null),
    };
    Ok(json!({"key":key,"revision":revision,"value":value}))
}

fn task_read(conn: &Connection, task: &TaskId) -> Result<Value> {
    let (owner, description, result): (String, String, Option<String>) = conn
        .query_row(
            "SELECT owner,description,result FROM tasks WHERE id=?1",
            [task.as_str()],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )
        .context("unknown task")?;
    Ok(json!({"task":task,"owner":owner,"description":description,
        "result":result.map(|s| serde_json::from_str::<Value>(&s)).transpose()?}))
}

fn bounded_text(text: &str, max: usize, name: &str) -> Result<()> {
    ensure!(
        !text.is_empty() && serde_json::to_string(text)?.len() <= max,
        "{name} must contain 1..{max} encoded bytes"
    );
    Ok(())
}
