mod verification_support;

use serde_json::{Value, json};
use std::time::{Duration, Instant};
use verification_support::*;

#[test]
fn every_mutation_recovers_at_each_commit_crash_boundary() {
    let start = Instant::now();
    let mut cases = Vec::new();
    for method in [
        "launch_start",
        "bind",
        "send",
        "inbox",
        "ack",
        "task_create",
        "task_finish",
        "cell_cas",
    ] {
        for crash in ["before_commit", "after_commit", "after_notify"] {
            let case_start = Instant::now();
            let harness = Harness::new();
            let actor = harness.actor("codex", "writer", "session");
            let mut task = Value::Null;
            let mut message = Value::Null;
            if matches!(method, "inbox" | "ack") {
                for n in 0..3 {
                    message = harness.call(actor.request(
                        json!({"method":"send","to":actor.member,"body":format!("message-{n}")}),
                    ))["message"]
                        .clone();
                }
                if method == "ack" {
                    harness.call(actor.request(json!({"method":"inbox","limit":3})));
                }
            }
            if method == "task_finish" {
                task = harness.call(actor.request(json!({"method":"task_create","owner":actor.member,"description":"crash completion"})))["task"].clone();
            }
            let request = match method {
                "launch_start" => setup(json!({"method":method,"provider":"claude","instance":"created"})),
                "bind" => actor.request(json!({"method":method,"session":"session","agent":"child","parent_agent":null})),
                "send" => actor.request(json!({"method":method,"to":actor.member,"body":"one durable effect"})),
                "inbox" => actor.request(json!({"method":method,"limit":2})),
                "ack" => actor.request(json!({"method":method,"through":message})),
                "task_create" => actor.request(json!({"method":method,"owner":actor.member,"description":"created once"})),
                "task_finish" => actor.request(json!({"method":method,"task":task,"result":{"status":"success","value":{"answer":42}}})),
                "cell_cas" => actor.request(json!({"method":method,"key":"revision","expected":0,"value":"committed"})),
                _ => unreachable!(),
            };
            let mut expected_counts = harness.row_counts();
            for table in match method {
                "launch_start" => &["launches", "members", "mailboxes"][..],
                "bind" => &["members", "mailboxes"][..],
                "send" => &["messages"][..],
                "task_create" => &["tasks"][..],
                "cell_cas" => &["cells"][..],
                _ => &[][..],
            } {
                *expected_counts.get_mut(*table).unwrap() += 1;
            }
            let output = harness.output(&request, &[("ICARUS_TEST_CRASH", crash)]);
            assert_eq!(
                output.status.code(),
                Some(86),
                "{method}/{crash}: {}",
                String::from_utf8_lossy(&output.stderr)
            );
            assert!(
                output.stdout.is_empty(),
                "{method}/{crash} replied before crash"
            );
            let history = harness.history();
            let durable: Vec<_> = history
                .as_array()
                .unwrap()
                .iter()
                .filter(|entry| entry["operation_id"] == request["operation_id"])
                .collect();
            let committed = crash != "before_commit";
            assert_eq!(durable.len(), usize::from(committed), "{method}/{crash}");
            let replay_result = durable.first().map(|entry| entry["result"].clone());

            match method {
                "bind" => {
                    let members = harness.call(actor.request(json!({"method":"members"})));
                    assert_eq!(
                        members["members"]
                            .as_array()
                            .unwrap()
                            .iter()
                            .filter(|m| m["agent"] == "child")
                            .count(),
                        usize::from(committed)
                    );
                }
                "send" | "ack" => {
                    let inbox = harness.call(actor.request(json!({"method":"inbox"})));
                    let expected = if method == "send" {
                        usize::from(committed)
                    } else if committed {
                        0
                    } else {
                        3
                    };
                    assert_eq!(
                        inbox["messages"].as_array().unwrap().len(),
                        expected,
                        "{method}/{crash}"
                    );
                }
                "task_finish" => {
                    let result =
                        harness.call(actor.request(json!({"method":"task_read","task":task})));
                    assert_eq!(result["result"].is_null(), !committed, "{method}/{crash}");
                }
                "cell_cas" => {
                    let result =
                        harness.call(actor.request(json!({"method":"cell_read","key":"revision"})));
                    assert_eq!(result["revision"], u64::from(committed), "{method}/{crash}");
                }
                _ => {}
            }

            let recovered = harness.call(request.clone());
            if let Some(expected) = replay_result {
                assert_eq!(
                    recovered, expected,
                    "{method}/{crash} changed durable reply"
                );
            }
            assert_eq!(
                harness.call(request.clone()),
                recovered,
                "{method}/{crash} changed replay"
            );
            match method {
                "launch_start" => {
                    let child = Actor {
                        member: String::new(),
                        launch: recovered["launch"].as_str().unwrap().into(),
                    };
                    let bound = harness.call(child.request(json!({"method":"bind","session":"created-session","agent":null,"parent_agent":null})));
                    assert_eq!(bound["launch"], recovered["launch"]);
                }
                "bind" => {
                    let bound = harness.call(actor.request(request["command"].clone()));
                    assert_eq!(bound["member"], recovered["member"]);
                }
                "send" => {
                    let inbox = harness.call(actor.request(json!({"method":"inbox"})));
                    assert_eq!(inbox["messages"].as_array().unwrap().len(), 1);
                    assert_eq!(inbox["messages"][0]["message"], recovered["message"]);
                    assert_eq!(inbox["messages"][0]["sequence"], 1);
                }
                "inbox" => {
                    assert_eq!(recovered["messages"].as_array().unwrap().len(), 2);
                    assert_eq!(recovered["batch_end"], 2);
                    assert_eq!(
                        harness.call(actor.request(json!({"method":"inbox","limit":4}))),
                        recovered
                    );
                }
                "ack" => {
                    let inbox = harness.call(actor.request(json!({"method":"inbox"})));
                    assert_eq!(inbox["acked"], 3);
                    assert!(inbox["messages"].as_array().unwrap().is_empty());
                }
                "task_create" | "task_finish" => {
                    let task = if method == "task_create" {
                        recovered["task"].clone()
                    } else {
                        task
                    };
                    let read =
                        harness.call(actor.request(json!({"method":"task_read","task":task})));
                    assert_eq!(read["owner"], actor.member);
                    if method == "task_create" {
                        assert!(read["result"].is_null());
                    } else {
                        assert_eq!(read["result"], request["command"]["result"]);
                    }
                }
                "cell_cas" => {
                    let read =
                        harness.call(actor.request(json!({"method":"cell_read","key":"revision"})));
                    assert_eq!(read["revision"], 1);
                    assert_eq!(read["value"], "committed");
                }
                _ => unreachable!(),
            }
            let history = harness.history();
            assert_eq!(
                history
                    .as_array()
                    .unwrap()
                    .iter()
                    .filter(|entry| entry["operation_id"] == request["operation_id"])
                    .count(),
                1
            );
            assert_eq!(
                harness.row_counts(),
                expected_counts,
                "{method}/{crash} left a duplicate or missing physical effect"
            );
            harness.check_database();
            cases.push(json!({"mutation":method,"crash":crash,"committed_before_reply":committed,"identical_retries":2,"durable_effects":1,"integrity_check":"ok","foreign_key_violations":0,"elapsed_ms":case_start.elapsed().as_secs_f64()*1000.0}));
        }
    }
    assert_eq!(cases.len(), 24);
    report("mutation_crash_matrix", start, cases);
}

fn prepare_wait(harness: &Harness, actor: &Actor, kind: &str) -> (Value, Value) {
    match kind {
        "messages" => (
            actor.request(json!({"method":"wait_any","messages":true,"tasks":[],"cells":[],"timeout_ms":10000})),
            actor.request(json!({"method":"send","to":actor.member,"body":"wake"})),
        ),
        "tasks" => {
            let task = harness.call(actor.request(json!({"method":"task_create","owner":actor.member,"description":"wait target"})))["task"].clone();
            (
                actor.request(json!({"method":"wait_any","messages":false,"tasks":[task],"cells":[],"timeout_ms":10000})),
                actor.request(json!({"method":"task_finish","task":task,"result":{"status":"success","value":"done"}})),
            )
        }
        "cells" => (
            actor.request(json!({"method":"wait_any","messages":false,"tasks":[],"cells":[{"key":"wake","revision":0}],"timeout_ms":10000})),
            actor.request(json!({"method":"cell_cas","key":"wake","expected":0,"value":"changed"})),
        ),
        _ => unreachable!(),
    }
}

fn assert_ready(kind: &str, result: &Value) {
    assert_eq!(result["status"], "ready", "{kind}: {result}");
    match kind {
        "messages" => assert_eq!(result["ready"]["messages"], true),
        "tasks" => {
            assert_eq!(result["ready"]["tasks"].as_array().unwrap().len(), 1);
            assert_eq!(result["ready"]["tasks"][0]["result"]["status"], "success");
        }
        "cells" => {
            assert_eq!(result["ready"]["cells"].as_array().unwrap().len(), 1);
            assert_eq!(result["ready"]["cells"][0]["revision"], 1);
        }
        _ => unreachable!(),
    }
}

#[test]
fn wait_schedules_observe_commits_before_and_after_each_read_boundary() {
    let start = Instant::now();
    let mut cases = Vec::new();
    for kind in ["messages", "tasks", "cells"] {
        for point in ["before_arm", "after_arm", "after_empty_read"] {
            for dropped in [false, true] {
                let harness = Harness::new();
                let actor = harness.actor("codex", "waiter", "session");
                let (wait, mutation) = prepare_wait(&harness, &actor, kind);
                let barrier = Barrier::new(point);
                let mut waiter = harness.spawn("api", Some(&wait), &barrier.env());
                barrier.ready(&mut waiter);
                let wake_targets = harness.bound_wake_targets();
                if point == "before_arm" {
                    assert_eq!(wake_targets, 0, "notification must precede arming");
                } else if std::env::var_os("ICARUS_REQUIRE_DATAGRAM").is_some() {
                    assert_eq!(
                        wake_targets, 1,
                        "datagram campaign requires an armed socket"
                    );
                }
                let notification_path = harness.home.path().join("notification.json");
                let mut env = if dropped {
                    vec![("ICARUS_TEST_DROP_WAKEUPS", "1")]
                } else {
                    vec![]
                };
                env.push((
                    "ICARUS_TEST_NOTIFY_REPORT",
                    notification_path.to_str().unwrap(),
                ));
                accepted(harness.output(&mutation, &env));
                let writer_replied = Instant::now();
                let notification: Value =
                    serde_json::from_slice(&std::fs::read(notification_path).unwrap()).unwrap();
                assert_eq!(notification["dropped"], dropped);
                if dropped || point == "before_arm" {
                    assert_eq!(notification["sent"], 0);
                } else if std::env::var_os("ICARUS_REQUIRE_DATAGRAM").is_some() {
                    assert!(
                        notification["sent"].as_u64().unwrap() >= 1,
                        "no hint queued to the armed waiter: {notification}"
                    );
                }
                assert!(
                    waiter.0.try_wait().unwrap().is_none(),
                    "waiter left its barrier before release"
                );
                barrier.release();
                let result = accepted(waiter.finish(Duration::from_secs(5)));
                let observation_ms = writer_replied.elapsed().as_secs_f64() * 1000.0;
                assert_ready(kind, &result);
                harness.check_database();
                cases.push(json!({"wait_for":kind,"pause":point,"notification_policy":if dropped {"all_dropped"} else {"enabled"},"notification":notification,"bound_wake_targets_at_commit":wake_targets,"schedule":"waiter reaches marker; writer commits; waiter resumes","writer_response_to_waiter_response_ms":observation_ms,"result":"ready"}));
            }
        }
    }
    assert_eq!(cases.len(), 18);
    report("forced_wait_schedules", start, cases);
}

#[test]
fn cancellation_and_disconnect_stop_only_the_pending_wait() {
    let start = Instant::now();
    let mut cases = Vec::new();
    for ending in ["cancel", "disconnect"] {
        let harness = Harness::new();
        let actor = harness.actor("codex", "waiter", "session");
        let (wait, mutation) = prepare_wait(&harness, &actor, "messages");
        let barrier = Barrier::new("after_empty_read");
        let mut mcp = Mcp::new(&harness, &actor, &barrier.env());
        let mut arguments = wait["command"].as_object().unwrap().clone();
        arguments.remove("method");
        arguments.insert("member".into(), json!(actor.member));
        arguments.insert("launch".into(), json!(actor.launch));
        mcp.send(json!({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait_any","arguments":arguments}}));
        barrier.ready(&mut mcp.process);
        if ending == "cancel" {
            mcp.send(json!({"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}));
            mcp.send(json!({"jsonrpc":"2.0","id":2,"method":"ping"}));
            assert_eq!(
                mcp.receive()["id"],
                2,
                "cancel must be read while the wait is paused"
            );
        } else {
            drop(mcp.input.take());
        }
        barrier.release();
        let response = mcp.receive();
        assert_eq!(response["id"], 1);
        let result: Value =
            serde_json::from_str(response["result"]["content"][0]["text"].as_str().unwrap())
                .unwrap();
        assert_eq!(result["result"]["status"], "cancelled", "{result}");
        mcp.finish();
        harness.call(mutation);
        let mut poll = wait;
        poll["command"]["timeout_ms"] = json!(0);
        assert_ready("messages", &harness.call(poll));
        harness.check_database();
        cases.push(json!({"ending":ending,"wait_paused_after_empty_read":true,"wait_result":"cancelled","later_message":"ready"}));
    }
    report("wait_cancellation_and_disconnect", start, cases);
}

#[test]
fn killed_waiters_recover_all_wait_kinds_without_notifications() {
    let start = Instant::now();
    let mut cases = Vec::new();
    for kind in ["messages", "tasks", "cells"] {
        let harness = Harness::new();
        let actor = harness.actor("codex", "waiter", "session");
        let (mut wait, mutation) = prepare_wait(&harness, &actor, kind);
        let barrier = Barrier::new("after_empty_read");
        let mut waiter = harness.spawn("api", Some(&wait), &barrier.env());
        barrier.ready(&mut waiter);
        waiter.0.kill().unwrap();
        let killed = waiter.finish(Duration::from_secs(5));
        assert!(!killed.status.success());
        assert!(killed.stdout.is_empty());
        accepted(harness.output(&mutation, &[("ICARUS_TEST_DROP_WAKEUPS", "1")]));
        wait["command"]["timeout_ms"] = json!(0);
        assert_ready(kind, &harness.call(wait));
        harness.check_database();
        cases.push(json!({"wait_for":kind,"kill_point":"after_empty_read","notification_policy":"all_dropped","restarted_wait":"ready"}));
    }
    report("waiter_restart", start, cases);
}

#[test]
fn cold_database_startup_allows_concurrent_helpers() {
    let start = Instant::now();
    let rounds = std::env::var("ICARUS_STARTUP_ROUNDS")
        .ok()
        .map(|n| n.parse::<usize>().unwrap())
        .unwrap_or(10);
    let width = 32;
    let mut cases = Vec::new();
    for round in 0..rounds {
        let harness = Harness::new();
        let mut children = Vec::new();
        let mut barriers = Vec::new();
        for n in 0..width {
            let barrier = Barrier::new("before_open");
            let request = setup(
                json!({"method":"launch_start","provider":"codex","instance":format!("cold-{n}")}),
            );
            let process = harness.spawn("api", Some(&request), &barrier.env());
            children.push(process);
            barriers.push(barrier);
        }
        for (barrier, process) in barriers.iter().zip(&mut children) {
            barrier.ready(process);
        }
        let released = Instant::now();
        for barrier in &barriers {
            barrier.release();
        }
        let mut launches = std::collections::HashSet::new();
        for process in children {
            let result = accepted(process.finish(Duration::from_secs(15)));
            assert!(launches.insert(result["launch"].as_str().unwrap().to_string()));
        }
        assert_eq!(harness.history().as_array().unwrap().len(), width);
        harness.check_database();
        cases.push(json!({"round":round,"helpers":width,"all_paused_before_open":true,"distinct_launches":launches.len(),"elapsed_after_release_ms":released.elapsed().as_secs_f64()*1000.0,"integrity_check":"ok","foreign_key_violations":0}));
    }
    report("cold_startup", start, cases);
}

#[test]
fn repeated_stop_text_can_deliver_the_next_acknowledgement_batch() {
    let start = Instant::now();
    let harness = Harness::new();
    let actor = harness.actor("claude", "worker", "session");
    for n in 0..5 {
        harness.call(
            actor.request(json!({"method":"send","to":actor.member,"body":format!("message-{n}")})),
        );
    }
    let event = json!({"hook_event_name":"Stop","session_id":"session","last_assistant_message":"Done.","stop_hook_active":false});
    let first = harness
        .hook(&actor, "claude", &event, &[])
        .finish(Duration::from_secs(5));
    assert!(first.status.success());
    let first: Value = serde_json::from_slice(&first.stdout).unwrap();
    assert_eq!(first["decision"], "block");
    let batch = harness.call(actor.request(json!({"method":"inbox"})));
    assert_eq!(batch["batch_end"], 4);
    let repeat = harness
        .hook(&actor, "claude", &event, &[])
        .finish(Duration::from_secs(5));
    assert!(repeat.status.success());
    assert_eq!(
        serde_json::from_slice::<Value>(&repeat.stdout).unwrap(),
        json!({})
    );
    harness.call(actor.request(json!({"method":"ack","through":batch["messages"][3]["message"]})));
    let next = harness
        .hook(&actor, "claude", &event, &[])
        .finish(Duration::from_secs(5));
    assert!(next.status.success());
    let next: Value = serde_json::from_slice(&next.stdout).unwrap();
    assert_eq!(
        next["decision"], "block",
        "identical assistant text must not suppress a later batch"
    );
    assert!(next["reason"].as_str().unwrap().contains("message-4"));
    assert!(!next["reason"].as_str().unwrap().contains("message-0"));
    let mut recursive = event;
    recursive["stop_hook_active"] = json!(true);
    let recursive = harness
        .hook(&actor, "claude", &recursive, &[])
        .finish(Duration::from_secs(5));
    assert_eq!(
        serde_json::from_slice::<Value>(&recursive.stdout).unwrap(),
        json!({})
    );
    harness.check_database();
    report(
        "stop_batch_progress",
        start,
        vec![
            json!({"messages":5,"batch_ends":[4,5],"same_text":true,"first_batch_repeat_suppressed":true,"second_batch_delivered":true,"native_recursion_suppressed":true}),
        ],
    );
}

#[test]
fn hook_fetch_ack_and_resume_races_keep_mailbox_and_identity_contracts() {
    let start = Instant::now();
    let mut cases = Vec::new();
    for racing_action in ["ack", "resume"] {
        let harness = Harness::new();
        let actor = harness.actor("claude", "worker", "session");
        let message =
            harness
                .call(actor.request(json!({"method":"send","to":actor.member,"body":"in flight"})))
                ["message"]
                .clone();
        let barrier = Barrier::new("before_injection");
        let event = json!({"hook_event_name":"PostToolUse","session_id":"session"});
        let mut hook = harness.hook(&actor, "claude", &event, &barrier.env());
        barrier.ready(&mut hook);
        if racing_action == "ack" {
            harness.call(actor.request(json!({"method":"ack","through":message})));
        } else {
            let resumed = harness.actor("claude", "worker", "session");
            assert_eq!(resumed.member, actor.member);
            assert_ne!(resumed.launch, actor.launch);
        }
        barrier.release();
        let output = hook.finish(Duration::from_secs(5));
        if racing_action == "ack" {
            assert!(
                output.status.success(),
                "{}",
                String::from_utf8_lossy(&output.stderr)
            );
            let injected: Value = serde_json::from_slice(&output.stdout).unwrap();
            assert!(
                injected["hookSpecificOutput"]["additionalContext"]
                    .as_str()
                    .unwrap()
                    .contains(message.as_str().unwrap())
            );
            assert!(
                harness.call(actor.request(json!({"method":"inbox"})))["messages"]
                    .as_array()
                    .unwrap()
                    .is_empty()
            );
        } else {
            assert!(
                !output.status.success(),
                "stale hook injected after a completed resume"
            );
            assert!(output.stdout.is_empty());
        }
        harness.check_database();
        cases.push(json!({"paused":"before_injection","racing_action":racing_action,"contract":if racing_action == "ack" {"already_fetched_output_may_arrive_after_ack"} else {"stale_launch_rejected_before_output"}}));
    }
    report("hook_delivery_races", start, cases);
}

#[test]
fn ancestor_resume_rejects_descendant_calls_and_replays_without_rebinding() {
    let start = Instant::now();
    let harness = Harness::new();
    let main = harness.actor("claude", "worker", "session");
    let mut actors = vec![main.clone()];
    let mut binds = Vec::new();
    for depth in 1..=4 {
        let request = main.request(json!({"method":"bind","session":"session","agent":format!("child-{depth}"),"parent_agent":if depth==1 {Value::Null} else {json!(format!("child-{}",depth-1))}}));
        let response = harness.call(request.clone());
        actors.push(Actor {
            member: response["member"].as_str().unwrap().into(),
            launch: main.launch.clone(),
        });
        binds.push((request, response));
    }
    let sends: Vec<_> = actors
        .iter()
        .map(|actor| {
            let request = actor.request(
                json!({"method":"send","to":actor.member,"body":"retained across ancestor resume"}),
            );
            let response = harness.call(request.clone());
            (request, response)
        })
        .collect();
    let barrier = Barrier::new("before_injection");
    let mut deep_hook = harness.hook(
        &actors[4],
        "claude",
        &json!({"hook_event_name":"PostToolUse","session_id":"session","agent_id":"child-4"}),
        &barrier.env(),
    );
    barrier.ready(&mut deep_hook);
    let resumed = harness.actor("claude", "worker", "session");
    assert_eq!(resumed.member, main.member);
    barrier.release();
    let stale_output = deep_hook.finish(Duration::from_secs(5));
    assert!(!stale_output.status.success());
    assert!(
        stale_output.stdout.is_empty(),
        "descendant hook injected after its ancestor resumed"
    );
    let mut cases = Vec::new();
    for (depth, actor) in actors.iter().enumerate() {
        for request in [
            actor.request(json!({"method":"members"})),
            sends[depth].0.clone(),
        ] {
            let output = harness.output(&request, &[]);
            assert!(
                !output.status.success(),
                "depth {depth} retained authority after resume"
            );
            let error: Value = serde_json::from_slice(&output.stdout).unwrap();
            assert_eq!(error["ok"], false);
            assert!(
                error["error"].as_str().unwrap().contains("stale launch"),
                "{error}"
            );
        }
        cases.push(
            json!({"depth":depth,"fresh_call":"stale_launch","mutation_replay":"stale_launch"}),
        );
    }
    for (request, response) in &binds {
        assert_eq!(
            harness.call(request.clone()),
            *response,
            "bind replay must retain its historical result"
        );
    }
    let members = harness.call(resumed.request(json!({"method":"members"})));
    for member in members["members"].as_array().unwrap() {
        assert_eq!(
            member["launch"],
            if member["member"] == main.member {
                json!(resumed.launch)
            } else {
                json!(main.launch)
            }
        );
    }
    let mut restored = vec![resumed.clone()];
    for (request, _) in binds {
        let response = harness.call(resumed.request(request["command"].clone()));
        restored.push(Actor {
            member: response["member"].as_str().unwrap().into(),
            launch: resumed.launch.clone(),
        });
    }
    for (depth, actor) in restored.iter().enumerate() {
        assert_eq!(actor.member, actors[depth].member);
        let inbox = harness.call(actor.request(json!({"method":"inbox"})));
        assert_eq!(inbox["messages"].as_array().unwrap().len(), 1);
        assert_eq!(inbox["messages"][0]["message"], sends[depth].1["message"]);
    }
    harness.check_database();
    report("ancestor_resume", start, cases);
}

#[test]
fn fetch_and_injection_do_not_acknowledge_mail() {
    let start = Instant::now();
    let mut cases = Vec::new();
    for crash in ["after_fetch", "after_injection"] {
        let harness = Harness::new();
        let actor = harness.actor("claude", "worker", "session");
        let message = harness.call(actor.request(
            json!({"method":"send","to":actor.member,"body":"must remain unacknowledged"}),
        ))["message"]
            .clone();
        let event = json!({"hook_event_name":"PostToolUse","session_id":"session"});
        let output = harness
            .hook(&actor, "claude", &event, &[("ICARUS_TEST_CRASH", crash)])
            .finish(Duration::from_secs(5));
        assert_eq!(output.status.code(), Some(86));
        if crash == "after_fetch" {
            assert!(output.stdout.is_empty());
        } else {
            assert!(String::from_utf8_lossy(&output.stdout).contains(message.as_str().unwrap()));
        }
        let inbox = harness.call(actor.request(json!({"method":"inbox"})));
        assert_eq!(
            inbox["acked"], 0,
            "fetch or injection acknowledged mail without ack"
        );
        assert_eq!(
            inbox["messages"].as_array().unwrap().len(),
            1,
            "fetch or injection acknowledged mail without ack"
        );
        assert_eq!(inbox["messages"][0]["message"], message);
        harness.call(actor.request(json!({"method":"ack","through":message})));
        let inbox = harness.call(actor.request(json!({"method":"inbox"})));
        assert_eq!(inbox["acked"], 1);
        assert!(inbox["messages"].as_array().unwrap().is_empty());
        harness.check_database();
        cases.push(json!({"crash":crash,"before_explicit_ack":0,"retained_messages":1,"after_explicit_ack":1}));
    }
    report("fetch_injection_without_ack", start, cases);
}
