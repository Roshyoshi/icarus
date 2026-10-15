use rusqlite::Connection;
use serde_json::{Value, json};
use std::{
    fs,
    io::{BufRead, BufReader, Read, Write},
    os::unix::fs::FileTypeExt,
    path::{Path, PathBuf},
    process::{Child, ChildStdin, Command, Output, Stdio},
    sync::mpsc::{self, Receiver},
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};
use tempfile::TempDir;

pub fn binary() -> PathBuf {
    std::env::var_os("ICARUS_SYNC_BIN")
        .map(PathBuf::from)
        .unwrap_or_else(|| env!("CARGO_BIN_EXE_icarus-sync").into())
}

pub fn id() -> String {
    uuid::Uuid::new_v4().to_string()
}

pub struct Harness {
    pub home: TempDir,
}

impl Harness {
    pub fn new() -> Self {
        Self {
            home: tempfile::tempdir().unwrap(),
        }
    }

    pub fn spawn(&self, mode: &str, input: Option<&Value>, env: &[(&str, &str)]) -> Process {
        let mut command = Command::new(binary());
        command
            .args([
                "--home",
                self.home.path().to_str().unwrap(),
                "--group",
                "verification",
                mode,
            ])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        for name in [
            "ICARUS_TEST_CRASH",
            "ICARUS_TEST_DROP_WAKEUPS",
            "ICARUS_TEST_ARMED",
            "ICARUS_TEST_PAUSE",
            "ICARUS_TEST_BARRIER_DIR",
            "ICARUS_TEST_NOTIFY_REPORT",
            "ICARUS_LAUNCH",
            "ICARUS_PROVIDER",
        ] {
            command.env_remove(name);
        }
        command.envs(env.iter().copied());
        let mut process = Process(command.spawn().unwrap());
        if let Some(input) = input {
            serde_json::to_writer(process.0.stdin.take().unwrap(), input).unwrap();
        }
        process
    }

    pub fn output(&self, request: &Value, env: &[(&str, &str)]) -> Output {
        self.spawn("api", Some(request), env)
            .finish(Duration::from_secs(15))
    }

    pub fn call(&self, request: Value) -> Value {
        accepted(self.output(&request, &[]))
    }

    pub fn launch(&self, provider: &str, instance: &str) -> Actor {
        let result = self.call(setup(
            json!({"method":"launch_start","provider":provider,"instance":instance}),
        ));
        Actor {
            member: String::new(),
            launch: result["launch"].as_str().unwrap().into(),
        }
    }

    pub fn actor(&self, provider: &str, instance: &str, session: &str) -> Actor {
        let mut actor = self.launch(provider, instance);
        actor.member =
            self.call(actor.request(
                json!({"method":"bind","session":session,"agent":null,"parent_agent":null}),
            ))["member"]
                .as_str()
                .unwrap()
                .into();
        actor
    }

    pub fn history(&self) -> Value {
        let mut process = self.spawn("history", None, &[]);
        drop(process.0.stdin.take());
        let output = process.finish(Duration::from_secs(15));
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        serde_json::from_slice(&output.stdout).unwrap()
    }

    pub fn check_database(&self) {
        let connection =
            Connection::open(self.home.path().join("groups/verification/coordination.db")).unwrap();
        let integrity: String = connection
            .query_row("PRAGMA integrity_check", [], |row| row.get(0))
            .unwrap();
        assert_eq!(integrity, "ok");
        let foreign_keys: usize = connection
            .prepare("PRAGMA foreign_key_check")
            .unwrap()
            .query_map([], |_| Ok(()))
            .unwrap()
            .count();
        assert_eq!(foreign_keys, 0, "foreign-key violations after recovery");
    }

    pub fn row_counts(&self) -> std::collections::BTreeMap<String, usize> {
        let connection =
            Connection::open(self.home.path().join("groups/verification/coordination.db")).unwrap();
        [
            "launches",
            "members",
            "mailboxes",
            "messages",
            "tasks",
            "cells",
        ]
        .into_iter()
        .map(|table| {
            let count: i64 = connection
                .query_row(&format!("SELECT count(*) FROM {table}"), [], |row| {
                    row.get(0)
                })
                .unwrap();
            (table.to_string(), usize::try_from(count).unwrap())
        })
        .collect()
    }

    pub fn bound_wake_targets(&self) -> usize {
        let directory = self.home.path().join("groups/verification/wake");
        let Ok(entries) = fs::read_dir(directory) else {
            return 0;
        };
        entries
            .map(|entry| {
                let entry = entry.unwrap();
                let socket = fs::read_to_string(entry.path()).unwrap();
                usize::from(fs::metadata(socket).unwrap().file_type().is_socket())
            })
            .sum()
    }

    pub fn hook(
        &self,
        actor: &Actor,
        provider: &str,
        event: &Value,
        extra: &[(&str, &str)],
    ) -> Process {
        let mut env = vec![
            ("ICARUS_LAUNCH", actor.launch.as_str()),
            ("ICARUS_PROVIDER", provider),
        ];
        env.extend_from_slice(extra);
        self.spawn("hook", Some(event), &env)
    }
}

#[derive(Clone)]
pub struct Actor {
    pub member: String,
    pub launch: String,
}

impl Actor {
    pub fn request(&self, command: Value) -> Value {
        json!({"version":1,"operation_id":id(),"member":if self.member.is_empty(){Value::Null}else{json!(self.member)},"launch":self.launch,"command":command})
    }
}

pub fn setup(command: Value) -> Value {
    json!({"version":1,"operation_id":id(),"command":command})
}

pub fn accepted(output: Output) -> Value {
    assert!(
        output.status.success(),
        "exit {:?}: stdout={} stderr={}",
        output.status.code(),
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    let response: Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(response["ok"], true, "{response}");
    response["result"].clone()
}

pub struct Process(pub Child);

impl Process {
    pub fn finish(mut self, deadline: Duration) -> Output {
        drop(self.0.stdin.take());
        let stdout = self.0.stdout.take().map(|mut pipe| {
            thread::spawn(move || {
                let mut data = Vec::new();
                pipe.read_to_end(&mut data).unwrap();
                data
            })
        });
        let stderr = self.0.stderr.take().map(|mut pipe| {
            thread::spawn(move || {
                let mut data = Vec::new();
                pipe.read_to_end(&mut data).unwrap();
                data
            })
        });
        let start = Instant::now();
        let status = loop {
            if let Some(status) = self.0.try_wait().unwrap() {
                break status;
            }
            assert!(
                start.elapsed() < deadline,
                "helper exceeded {deadline:?} deadline"
            );
            thread::sleep(Duration::from_millis(2));
        };
        Output {
            status,
            stdout: stdout.map(|h| h.join().unwrap()).unwrap_or_default(),
            stderr: stderr.map(|h| h.join().unwrap()).unwrap_or_default(),
        }
    }
}

impl Drop for Process {
    fn drop(&mut self) {
        if self.0.try_wait().ok().flatten().is_none() {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
}

pub struct Barrier {
    directory: TempDir,
    point: &'static str,
}

impl Barrier {
    pub fn new(point: &'static str) -> Self {
        Self {
            directory: tempfile::tempdir().unwrap(),
            point,
        }
    }
    pub fn env(&self) -> [(&str, &str); 2] {
        [
            ("ICARUS_TEST_PAUSE", self.point),
            (
                "ICARUS_TEST_BARRIER_DIR",
                self.directory.path().to_str().unwrap(),
            ),
        ]
    }
    pub fn ready(&self, process: &mut Process) {
        let deadline = Instant::now() + Duration::from_secs(12);
        while !self
            .directory
            .path()
            .join(format!("{}.ready", self.point))
            .exists()
        {
            if let Some(status) = process.0.try_wait().unwrap() {
                let mut error = String::new();
                if let Some(stderr) = process.0.stderr.as_mut() {
                    stderr.read_to_string(&mut error).unwrap();
                }
                panic!(
                    "helper exited {status} before {} barrier: {error}",
                    self.point
                );
            }
            assert!(
                Instant::now() < deadline,
                "helper did not reach {} barrier",
                self.point
            );
            thread::sleep(Duration::from_millis(2));
        }
        assert!(
            process.0.try_wait().unwrap().is_none(),
            "helper was not paused at {}",
            self.point
        );
    }
    pub fn release(&self) {
        fs::write(
            self.directory
                .path()
                .join(format!("{}.release", self.point)),
            b"release",
        )
        .unwrap();
    }
}

pub struct Mcp {
    pub process: Process,
    pub input: Option<ChildStdin>,
    responses: Receiver<Value>,
    reader: JoinHandle<()>,
}

impl Mcp {
    pub fn new(harness: &Harness, actor: &Actor, env: &[(&str, &str)]) -> Self {
        let mut extra = vec![("ICARUS_LAUNCH", actor.launch.as_str())];
        extra.extend_from_slice(env);
        let mut process = harness.spawn("mcp", None, &extra);
        let input = process.0.stdin.take();
        let output = process.0.stdout.take().unwrap();
        let (sender, responses) = mpsc::channel();
        let reader = thread::spawn(move || {
            for line in BufReader::new(output).lines() {
                let line = line.unwrap();
                if sender.send(serde_json::from_str(&line).unwrap()).is_err() {
                    break;
                }
            }
        });
        Self {
            process,
            input,
            responses,
            reader,
        }
    }
    pub fn send(&mut self, request: Value) {
        let input = self.input.as_mut().unwrap();
        writeln!(input, "{request}").unwrap();
        input.flush().unwrap();
    }
    pub fn receive(&self) -> Value {
        self.responses
            .recv_timeout(Duration::from_secs(5))
            .expect("MCP response deadline")
    }
    pub fn finish(mut self) {
        drop(self.input.take());
        let output = self.process.finish(Duration::from_secs(5));
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        self.reader.join().unwrap();
    }
}

pub fn report(name: &str, start: Instant, cases: Vec<Value>) {
    let Some(directory) = std::env::var_os("ICARUS_VERIFY_REPORT_DIR") else {
        return;
    };
    let directory = Path::new(&directory);
    fs::create_dir_all(directory).unwrap();
    let value = json!({"test":name,"binary":binary(),"case_count":cases.len(),"elapsed_ms":start.elapsed().as_secs_f64()*1000.0,"cases":cases});
    fs::write(
        directory.join(format!("{name}.json")),
        serde_json::to_vec_pretty(&value).unwrap(),
    )
    .unwrap();
}
