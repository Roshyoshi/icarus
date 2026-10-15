use crate::{store::Store, types::*, wake};
use anyhow::{Context, Result, ensure};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    io::{self, BufRead, Read, Write},
    path::PathBuf,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    thread,
};

pub fn serve(home: PathBuf, group: GroupId, launch: LaunchId) -> Result<()> {
    let writer = Arc::new(Mutex::new(io::stdout()));
    let mut pending: HashMap<String, (Arc<AtomicBool>, thread::JoinHandle<()>)> = HashMap::new();
    let mut input = io::stdin().lock();
    loop {
        let mut line = String::new();
        let bytes = input.by_ref().take(131073).read_line(&mut line)?;
        if bytes == 0 {
            break;
        }
        ensure!(
            line.len() <= 131072 && line.ends_with('\n'),
            "MCP frame exceeds 128 KiB"
        );
        let request: Value = match serde_json::from_str(&line) {
            Ok(v) => v,
            Err(_) => {
                send(
                    &writer,
                    json!({"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"invalid JSON"}}),
                );
                continue;
            }
        };
        let id = request.get("id").cloned();
        let method = request["method"].as_str().unwrap_or("");
        if method == "notifications/cancelled" {
            let key = request["params"]["requestId"].to_string();
            if let Some((flag, _)) = pending.get(&key) {
                flag.store(true, Ordering::Release);
            }
            continue;
        }
        let Some(id) = id else {
            continue;
        };
        let finished: Vec<_> = pending
            .iter()
            .filter(|(_, (_, h))| h.is_finished())
            .map(|(id, _)| id.clone())
            .collect();
        for key in finished {
            let (_, h) = pending.remove(&key).unwrap();
            let _ = h.join();
        }
        let result = match method {
            "initialize" => Ok(
                json!({"protocolVersion":"2024-11-05","capabilities":{"tools":{}},
                "serverInfo":{"name":"icarus-sync","version":env!("CARGO_PKG_VERSION")}}),
            ),
            "ping" => Ok(json!({})),
            "tools/list" => Ok(json!({"tools":tools()})),
            "tools/call" => {
                let parsed = tool_request(&request["params"], &launch);
                match parsed {
                    Ok(req) if matches!(req.command, Command::WaitAny { .. }) => {
                        let key = id.to_string();
                        if pending.len() >= 32 || pending.contains_key(&key) {
                            Err(anyhow::anyhow!("too many waits or duplicate request ID"))
                        } else {
                            let flag = Arc::new(AtomicBool::new(false));
                            let cancel = flag.clone();
                            let (home, group, writer) =
                                (home.clone(), group.clone(), writer.clone());
                            let handle = thread::spawn(move || {
                                let result = Store::open(&home, &group)
                                    .and_then(|mut s| wake::wait_any(&mut s, &req, &cancel));
                                send(
                                    &writer,
                                    json!({"jsonrpc":"2.0","id":id,"result":tool_result(result)}),
                                );
                            });
                            pending.insert(key, (flag, handle));
                            continue;
                        }
                    }
                    Ok(req) => Ok(tool_result(
                        Store::open(&home, &group).and_then(|mut s| s.execute(&req)),
                    )),
                    Err(e) => Ok(tool_result(Err(e))),
                }
            }
            _ => {
                send(
                    &writer,
                    json!({"jsonrpc":"2.0","id":id,"error":{"code":-32601,"message":"method not found"}}),
                );
                continue;
            }
        };
        let message = match result {
            Ok(value) => json!({"jsonrpc":"2.0","id":id,"result":value}),
            Err(e) => {
                json!({"jsonrpc":"2.0","id":id,"error":{"code":-32602,"message":e.to_string()}})
            }
        };
        send(&writer, message);
    }
    for (flag, _) in pending.values() {
        flag.store(true, Ordering::Release);
    }
    for (_, (_, handle)) in pending {
        let _ = handle.join();
    }
    Ok(())
}

fn send(writer: &Arc<Mutex<io::Stdout>>, value: Value) {
    let mut out = writer.lock().unwrap();
    if writeln!(out, "{value}").and_then(|_| out.flush()).is_err() {
        std::process::exit(0);
    }
}

fn tool_result(result: Result<Value>) -> Value {
    let response = Response::from_result(result);
    json!({"isError":!response.ok,"content":[{"type":"text","text":serde_json::to_string(&response).unwrap()}]})
}

fn tool_request(params: &Value, launch: &LaunchId) -> Result<Request> {
    let name = params["name"].as_str().context("tool name required")?;
    ensure!(tools().iter().any(|t| t["name"] == name), "unknown tool");
    let args = params["arguments"]
        .as_object()
        .context("arguments must be an object")?;
    let mut command = args.clone();
    let member = command
        .remove("member")
        .context("explicit member required")?;
    let actual_launch = command
        .remove("launch")
        .context("explicit launch required")?;
    ensure!(
        actual_launch.as_str() == Some(launch.as_str()),
        "tool belongs to another launch"
    );
    let operation = command.remove("operation_id").unwrap_or(Value::Null);
    command.insert("method".into(), json!(name));
    Ok(serde_json::from_value(
        json!({"version":1,"member":member,"launch":actual_launch,
        "operation_id":operation,"command":command}),
    )?)
}

pub fn tools() -> Vec<Value> {
    let string = json!({"type":"string"});
    let mut tools = Vec::new();
    for (name, description, properties, required, mutation) in [
        (
            "members",
            "List registered members and their parent IDs.",
            json!({}),
            vec![],
            false,
        ),
        (
            "send",
            "Durably send a message (at most 2048 JSON-encoded bytes).",
            json!({"to":string,"body":string}),
            vec!["to", "body"],
            true,
        ),
        (
            "inbox",
            "Fetch an ordered prefix, up to 4 messages. Outstanding prefixes must be acknowledged before advancing.",
            json!({"limit":{"type":"integer","minimum":1,"maximum":4}}),
            vec![],
            true,
        ),
        (
            "ack",
            "Acknowledge a consumed prefix through a message ID in your mailbox.",
            json!({"through":string}),
            vec!["through"],
            true,
        ),
        (
            "task_create",
            "Create a task assigned to a member; returns a unique handle.",
            json!({"owner":string,"description":string}),
            vec!["owner", "description"],
            true,
        ),
        (
            "task_read",
            "Read a task's explicit terminal result or pending state.",
            json!({"task":string}),
            vec!["task"],
            false,
        ),
        (
            "task_finish",
            "Owner explicitly finishes a task. Terminal results are immutable.",
            json!({"task":string,"result":{"type":"object","properties":{"status":{"enum":["success","failure","cancelled"]},"value":{}},"required":["status","value"],"additionalProperties":false}}),
            vec!["task", "result"],
            true,
        ),
        (
            "cell_read",
            "Read a cell. Missing cells have revision 0 and null value.",
            json!({"key":string}),
            vec!["key"],
            false,
        ),
        (
            "cell_cas",
            "Replace a cell only at the expected revision; increments once on success.",
            json!({"key":string,"expected":{"type":"integer","minimum":0},"value":{}}),
            vec!["key", "expected", "value"],
            true,
        ),
        (
            "wait_any",
            "Wait for mailbox, task completion, or cell revision changes. Timeout/cancellation affects only this wait.",
            json!({"messages":{"type":"boolean"},"tasks":{"type":"array","items":string},"cells":{"type":"array","items":{"type":"object","properties":{"key":string,"revision":{"type":"integer","minimum":0}},"required":["key","revision"],"additionalProperties":false}},"timeout_ms":{"type":"integer","minimum":0,"maximum":86400000}}),
            vec!["messages", "tasks", "cells", "timeout_ms"],
            false,
        ),
    ] {
        let mut fields = properties.as_object().unwrap().clone();
        fields.insert("member".into(), string.clone());
        fields.insert("launch".into(), string.clone());
        let mut required = required;
        required.extend(["member", "launch"]);
        if mutation {
            fields.insert("operation_id".into(), string.clone());
            required.push("operation_id");
        }
        tools.push(json!({"name":name,"description":description,"inputSchema":{
            "type":"object","properties":fields,"required":required,"additionalProperties":false}}));
    }
    tools
}
