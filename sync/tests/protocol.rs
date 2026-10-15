mod common;
use common::*;
use serde_json::{Value, json};
use std::{
    fs, thread,
    time::{Duration, Instant},
};

#[test]
fn mailbox_prefix_ack_and_replay_survive_restart() {
    let h = Harness::new();
    let a = h.actor("codex", "a", "s", None);
    let b = h.actor("claude", "b", "s", None);
    let first = a.req(json!({"method":"send","to":b.member,"body":"first"}));
    let result = h.call(first.clone());
    assert_eq!(result, h.call(first.clone()));
    let mut changed = first;
    changed["command"]["body"] = json!("different");
    assert!(!api(h.dir.path(), &changed, &[]).status.success());
    for n in 2..=6 {
        h.call(a.req(json!({"method":"send","to":b.member,"body":n.to_string()})));
    }
    let inbox = b.req(json!({"method":"inbox","limit":2}));
    let batch = h.call(inbox.clone());
    assert_eq!(batch["messages"].as_array().unwrap().len(), 2);
    assert_eq!(batch["messages"][0]["message"], result["message"]);
    let again = h.call(b.req(json!({"method":"inbox","limit":4})));
    assert_eq!(batch, again);
    assert!(
        !api(
            h.dir.path(),
            &a.req(json!({"method":"ack","through":result["message"]})),
            &[]
        )
        .status
        .success()
    );
    h.call(b.req(json!({"method":"ack","through":batch["messages"][0]["message"]})));
    let partial = h.call(b.req(json!({"method":"inbox"})));
    assert_eq!(partial["messages"][0]["sequence"], 2);
    assert_eq!(partial["messages"].as_array().unwrap().len(), 1);
    h.call(b.req(json!({"method":"ack","through":batch["messages"][1]["message"]})));
    let next = h.call(b.req(json!({"method":"inbox"})));
    assert_eq!(next["messages"][0]["sequence"], 3);
    assert_eq!(
        h.call(inbox),
        batch,
        "replay is the same historical fetched output, even after ack"
    );
}

#[test]
fn crash_before_and_after_commit_preserves_one_effect() {
    for point in ["before_commit", "after_commit", "after_notify"] {
        let h = Harness::new();
        let a = h.actor("codex", "a", "s", None);
        let req = a.req(json!({"method":"send","to":a.member,"body":"durable"}));
        let output = api(h.dir.path(), &req, &[("ICARUS_TEST_CRASH", point)]);
        assert_eq!(output.status.code(), Some(86));
        assert!(output.stdout.is_empty());
        let count = h
            .store()
            .history()
            .unwrap()
            .as_array()
            .unwrap()
            .iter()
            .filter(|v| v["operation_id"] == req["operation_id"])
            .count();
        assert_eq!(count, usize::from(point != "before_commit"));
        let recovered = h.call(req.clone());
        assert_eq!(recovered, h.call(req));
        let batch = h.call(a.req(json!({"method":"inbox"})));
        assert_eq!(batch["messages"].as_array().unwrap().len(), 1);
        assert_eq!(batch["messages"][0]["sequence"], 1);
    }
}

#[test]
fn competing_cas_and_completion_have_one_winner() {
    let h = Harness::new();
    let a = h.actor("codex", "a", "s", None);
    let children: Vec<_> = (0..8)
        .map(|n| {
            start(
                h.dir.path(),
                &a.req(json!({"method":"cell_cas","key":"owner","expected":0,"value":n})),
                &[],
            )
        })
        .collect();
    let results: Vec<_> = children
        .into_iter()
        .map(|c| response(c.wait_with_output().unwrap()))
        .collect();
    assert_eq!(
        results
            .iter()
            .filter(|v| v["result"]["swapped"] == true)
            .count(),
        1,
        "{results:?}"
    );
    assert!(
        results
            .iter()
            .all(|v| v["ok"] == true && v["result"]["cell"]["revision"] == 1)
    );
    let task = h.call(a.req(json!({"method":"task_create","owner":a.member,"description":"race"})))
        ["task"]
        .clone();
    let children:Vec<_>=(0..2).map(|n|start(h.dir.path(),&a.req(json!({"method":"task_finish","task":task,"result":{"status":"success","value":n}})),&[])).collect();
    let results: Vec<_> = children
        .into_iter()
        .map(|c| response(c.wait_with_output().unwrap()))
        .collect();
    assert_eq!(
        results.iter().filter(|v| v["ok"] == true).count(),
        1,
        "{results:?}"
    );
    let old = h.call(a.req(json!({"method":"task_read","task":task})));
    assert_eq!(
        old,
        h.call(a.req(json!({"method":"task_read","task":task})))
    );
    let b = h.actor("claude", "b", "s", None);
    assert!(
        !api(
            h.dir.path(),
            &b.req(json!({"method":"task_finish","task":task,"result":old["result"]})),
            &[]
        )
        .status
        .success()
    );
}

#[test]
fn dropped_notifications_timeout_and_restart_do_not_lose_work() {
    let h = Harness::new();
    let a = h.actor("codex", "a", "s", None);
    let armed = h.dir.path().join("armed");
    let request =
        a.req(json!({"method":"wait_any","messages":true,"tasks":[],"cells":[],"timeout_ms":4000}));
    let child = start(
        h.dir.path(),
        &request,
        &[("ICARUS_TEST_ARMED", armed.to_str().unwrap())],
    );
    let deadline = Instant::now() + Duration::from_secs(5);
    while !armed.exists() {
        assert!(Instant::now() < deadline);
        thread::sleep(Duration::from_millis(5));
    }
    let sent = Instant::now();
    let result = api(
        h.dir.path(),
        &a.req(json!({"method":"send","to":a.member,"body":"wake"})),
        &[("ICARUS_TEST_DROP_WAKEUPS", "1")],
    );
    assert!(result.status.success());
    let ready = response(child.wait_with_output().unwrap());
    assert_eq!(ready["result"]["status"], "ready");
    assert!(sent.elapsed() < Duration::from_secs(3));
    let task = h.call(
        a.req(json!({"method":"task_create","owner":a.member,"description":"still pending"})),
    )["task"]
        .clone();
    let timeout = h.call(a.req(
        json!({"method":"wait_any","messages":false,"tasks":[task],"cells":[],"timeout_ms":0}),
    ));
    assert_eq!(timeout["status"], "timeout");
    assert_eq!(
        h.call(a.req(json!({"method":"task_read","task":task})))["result"],
        Value::Null
    );
    h.call(a.req(json!({"method":"cell_cas","key":"version","expected":0,"value":true})));
    assert_eq!(h.call(a.req(json!({"method":"wait_any","messages":false,"tasks":[],"cells":[{"key":"version","revision":0}],"timeout_ms":0})))["status"],"ready");
}

#[test]
fn identity_scopes_siblings_providers_resumes_and_stale_descendants() {
    let h = Harness::new();
    let main = h.actor("codex", "a", "same-session", None);
    let mut child = main.clone();
    child.member = h.call(main.req(
        json!({"method":"bind","session":"same-session","agent":"child","parent_agent":null}),
    ))["member"]
        .as_str()
        .unwrap()
        .into();
    let mut grandchild = main.clone();
    grandchild.member=h.call(main.req(json!({"method":"bind","session":"same-session","agent":"grandchild","parent_agent":"child"})))["member"].as_str().unwrap().into();
    let sibling = h.call(main.req(
        json!({"method":"bind","session":"same-session","agent":"sibling","parent_agent":null}),
    ))["member"]
        .clone();
    let other = h.actor("claude", "a", "same-session", Some("child"));
    assert_ne!(main.member, child.member);
    assert_ne!(json!(child.member), sibling);
    assert_ne!(other.member, child.member);
    h.call(other.req(json!({"method":"send","to":child.member,"body":"survives resume"})));
    let resumed = h.actor("codex", "a", "same-session", None);
    assert_eq!(resumed.member, main.member);
    assert_ne!(resumed.launch, main.launch);
    for stale in [&main, &child, &grandchild] {
        assert!(
            !api(h.dir.path(), &stale.req(json!({"method":"members"})), &[])
                .status
                .success()
        );
    }
    assert!(
        !api(
            h.dir.path(),
            &main.req(
                json!({"method":"bind","session":"same-session","agent":null,"parent_agent":null})
            ),
            &[]
        )
        .status
        .success()
    );
    let member = h.call(resumed.req(
        json!({"method":"bind","session":"same-session","agent":"child","parent_agent":null}),
    ))["member"]
        .as_str()
        .unwrap()
        .to_string();
    assert_eq!(member, child.member);
    child.launch = resumed.launch;
    assert_eq!(
        h.call(child.req(json!({"method":"inbox"})))["messages"][0]["body"],
        "survives resume"
    );
}

#[test]
fn protocol_rejects_invalid_ids_versions_and_unfetched_acks() {
    let h = Harness::new();
    let a = h.actor("codex", "a", "s", None);
    let message =
        h.call(a.req(json!({"method":"send","to":a.member,"body":"not fetched"})))["message"]
            .clone();
    for mut req in [
        a.req(json!({"method":"ack","through":message})),
        a.req(json!({"method":"inbox","limit":5})),
    ] {
        assert!(!api(h.dir.path(), &req, &[]).status.success());
        req["version"] = json!(2);
        assert!(!api(h.dir.path(), &req, &[]).status.success());
    }
    let mut req = a.req(json!({"method":"members"}));
    req["member"] = Value::Null;
    assert!(!api(h.dir.path(), &req, &[]).status.success());
    assert!(fs::metadata(h.dir.path().join("groups/test/coordination.db")).is_ok());
}
