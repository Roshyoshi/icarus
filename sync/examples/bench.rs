use icarus_sync::{store::Store, types::*, wake::Waiter};
use serde_json::{Value, json};
use std::{
    io::Write,
    process::{Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, Instant},
};

fn request(member: Option<&MemberId>, launch: Option<&LaunchId>, command: Value) -> Request {
    serde_json::from_value(json!({"version":1,"operation_id":OperationId::new(),
        "member":member,"launch":launch,"command":command}))
    .unwrap()
}

fn percentiles(mut values: Vec<f64>) -> Value {
    values.sort_by(f64::total_cmp);
    json!({"samples":values.len(),"p50_us":values[values.len()/2],
        "p95_us":values[(values.len()*95/100).min(values.len()-1)],
        "min_us":values.first(),"max_us":values.last(),"sorted_samples_us":values})
}

fn main() -> anyhow::Result<()> {
    let home = tempfile::tempdir()?;
    let group = GroupId::try_from("bench".to_string())?;
    let mut writer = Store::open(home.path(), &group)?;
    let launch: LaunchId = serde_json::from_value(
        writer.execute(&request(
            None,
            None,
            json!({"method":"launch_start","provider":"claude","instance":"bench"}),
        ))?["launch"]
            .clone(),
    )?;
    let member: MemberId = serde_json::from_value(
        writer.execute(&request(
            None,
            Some(&launch),
            json!({"method":"bind","session":"bench-session","agent":null,"parent_agent":null}),
        ))?["member"]
            .clone(),
    )?;
    let count = 500;
    let start = Instant::now();
    for n in 0..count {
        writer.execute(&request(
            Some(&member),
            Some(&launch),
            json!({"method":"cell_cas","key":"throughput","expected":n,"value":n}),
        ))?;
    }
    let throughput = count as f64 / start.elapsed().as_secs_f64();
    let mut reader = Store::open(home.path(), &group)?;
    let (armed_tx, armed_rx) = mpsc::sync_channel(0);
    let (observed_tx, observed_rx) = mpsc::sync_channel(0);
    let (m, l) = (member.clone(), launch.clone());
    let samples = 25;
    let handle = thread::spawn(move || -> anyhow::Result<()> {
        for n in 0..samples {
            let waiter = Waiter::arm(&reader.directory).ok();
            let req = request(
                Some(&m),
                Some(&l),
                json!({"method":"wait_any","messages":false,"tasks":[],"cells":[{"key":"wake","revision":n}],"timeout_ms":5000}),
            );
            assert!(reader.ready(&req)?["cells"].as_array().unwrap().is_empty());
            armed_tx.send(waiter.is_some())?;
            let deadline = Instant::now() + Duration::from_secs(5);
            loop {
                if !reader.ready(&req)?["cells"].as_array().unwrap().is_empty() {
                    break;
                }
                anyhow::ensure!(Instant::now() < deadline, "wake benchmark deadline");
                if let Some(w) = &waiter {
                    w.sleep(Duration::from_secs(1))?;
                } else {
                    thread::sleep(Duration::from_secs(1));
                }
            }
            observed_tx.send(Instant::now())?;
        }
        Ok(())
    });
    let mut latency = Vec::new();
    let mut notifications = 0;
    for n in 0..samples {
        notifications += usize::from(armed_rx.recv()?);
        let (_, committed) = writer.execute_timed(&request(
            Some(&member),
            Some(&launch),
            json!({"method":"cell_cas","key":"wake","expected":n,"value":n}),
        ))?;
        let observed = observed_rx.recv()?;
        let committed = committed.unwrap();
        // Visibility may precede the writer returning from COMMIT. Keep that
        // signed difference instead of silently clamping it to zero.
        latency.push(if observed >= committed {
            observed.duration_since(committed).as_secs_f64() * 1e6
        } else {
            -committed.duration_since(observed).as_secs_f64() * 1e6
        });
    }
    handle.join().unwrap()?;
    let binary = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "target/release/icarus-sync".into());
    let mut hook_us = Vec::new();
    for _ in 0..25 {
        let start = Instant::now();
        let mut child = Command::new(&binary)
            .args([
                "--home",
                home.path().to_str().unwrap(),
                "--group",
                "bench",
                "--launch",
                launch.as_str(),
                "--provider",
                "claude",
                "hook",
            ])
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::inherit())
            .spawn()?;
        child
            .stdin
            .take()
            .unwrap()
            .write_all(br#"{"hook_event_name":"PostToolUse","session_id":"bench-session"}"#)?;
        anyhow::ensure!(child.wait()?.success(), "hook failed");
        hook_us.push(start.elapsed().as_secs_f64() * 1e6);
    }
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "platform":std::env::consts::OS,"architecture":std::env::consts::ARCH,
            "sqlite_version":rusqlite::version(),
            "storage":"SQLite WAL synchronous=FULL fullfsync=ON, temporary local filesystem",
            "throughput":{"durable_cas":count,"operations_per_second":throughput},
            "commit_to_observation":percentiles(latency),"waiters_with_datagram_socket":notifications,
            "hook_process_empty_mailbox":percentiles(hook_us),
            "model_latency_included":false,
            "scope":"one writer, one reader, warm connections; hook includes process startup and DB open"
        }))?
    );
    Ok(())
}
