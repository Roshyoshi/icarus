mod common;
use common::*;
use serde_json::{Value, json};
use std::{
    io::{BufRead, BufReader, Write},
    sync::mpsc,
    thread,
    time::Duration,
};

#[test]
fn mcp_tools_require_identity_and_wait_cancellation_is_local() {
    let h = Harness::new();
    let a = h.actor("codex", "a", "s", None);
    let mut child = child(h.dir.path(), "mcp", &[("ICARUS_LAUNCH", a.launch.as_str())]);
    let mut input = child.stdin.take().unwrap();
    let output = child.stdout.take().unwrap();
    let (tx, rx) = mpsc::channel();
    let reader = thread::spawn(move || {
        for line in BufReader::new(output).lines() {
            tx.send(serde_json::from_str::<Value>(&line.unwrap()).unwrap())
                .unwrap();
        }
    });
    let mut send = |value: Value| {
        writeln!(input, "{value}").unwrap();
        input.flush().unwrap();
    };
    let receive = || {
        rx.recv_timeout(Duration::from_secs(5))
            .expect("MCP response deadline")
    };
    send(
        json!({"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}),
    );
    assert_eq!(receive()["result"]["serverInfo"]["name"], "icarus-sync");
    send(json!({"jsonrpc":"2.0","method":"notifications/initialized"}));
    send(json!({"jsonrpc":"2.0","id":2,"method":"tools/list"}));
    assert_eq!(receive()["result"]["tools"].as_array().unwrap().len(), 10);
    send(
        json!({"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"members","arguments":{}}}),
    );
    assert_eq!(receive()["result"]["isError"], true);
    send(
        json!({"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"wait_any","arguments":{"member":a.member,"launch":a.launch,"messages":true,"tasks":[],"cells":[],"timeout_ms":60000}}}),
    );
    send(json!({"jsonrpc":"2.0","id":5,"method":"ping"}));
    assert_eq!(
        receive()["id"],
        5,
        "pending wait must not block protocol input"
    );
    send(
        json!({"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":4,"reason":"test"}}),
    );
    let cancelled = receive();
    assert_eq!(cancelled["id"], 4);
    let result: Value =
        serde_json::from_str(cancelled["result"]["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(result["result"]["status"], "cancelled", "{result}");
    send(
        json!({"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"send","arguments":{"member":a.member,"launch":a.launch,"operation_id":id(),"to":a.member,"body":"after cancellation"}}}),
    );
    assert_eq!(receive()["result"]["isError"], false);
    send(
        json!({"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"members","arguments":{"member":a.member,"launch":"wrong-launch"}}}),
    );
    assert_eq!(receive()["result"]["isError"], true);
    drop(input);
    assert!(child.wait().unwrap().success());
    reader.join().unwrap();
    assert_eq!(
        h.call(a.req(json!({"method":"inbox"})))["messages"][0]["body"],
        "after cancellation"
    );
}
