#![allow(dead_code)]
use icarus_sync::{store::Store, types::*};
use serde_json::{Value, json};
use std::{
    io::Write,
    path::Path,
    process::{Child, Command, Output, Stdio},
};
use tempfile::TempDir;

pub const BIN: &str = env!("CARGO_BIN_EXE_icarus-sync");
pub struct Harness {
    pub dir: TempDir,
}
impl Harness {
    pub fn new() -> Self {
        Self {
            dir: tempfile::tempdir().unwrap(),
        }
    }
    pub fn store(&self) -> Store {
        Store::open(
            self.dir.path(),
            &GroupId::try_from("test".to_string()).unwrap(),
        )
        .unwrap()
    }
    pub fn call(&self, request: Value) -> Value {
        let output = api(self.dir.path(), &request, &[]);
        assert!(
            output.status.success(),
            "{} {}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
        response(output)["result"].clone()
    }
    pub fn actor(
        &self,
        provider: &str,
        instance: &str,
        session: &str,
        agent: Option<&str>,
    ) -> Actor {
        let launch=self.call(json!({"version":1,"operation_id":id(),"command":{"method":"launch_start","provider":provider,"instance":instance}}))["launch"].as_str().unwrap().to_string();
        let mut actor = Actor {
            member: String::new(),
            launch,
        };
        let main = self.call(
            actor.req(json!({"method":"bind","session":session,"agent":null,"parent_agent":null})),
        )["member"]
            .as_str()
            .unwrap()
            .to_string();
        actor.member = main;
        if let Some(agent) = agent {
            actor.member =
                self.call(actor.req(
                    json!({"method":"bind","session":session,"agent":agent,"parent_agent":null}),
                ))["member"]
                    .as_str()
                    .unwrap()
                    .to_string();
        }
        actor
    }
}

#[derive(Clone)]
pub struct Actor {
    pub member: String,
    pub launch: String,
}
impl Actor {
    pub fn req(&self, command: Value) -> Value {
        json!({"version":1,"operation_id":id(),"member":if self.member.is_empty(){Value::Null}else{json!(self.member)},"launch":self.launch,"command":command})
    }
}
pub fn id() -> String {
    uuid::Uuid::new_v4().to_string()
}
pub fn child(home: &Path, mode: &str, env: &[(&str, &str)]) -> Child {
    let mut cmd = Command::new(BIN);
    cmd.args(["--home", home.to_str().unwrap(), "--group", "test", mode])
        .env_remove("ICARUS_TEST_CRASH")
        .env_remove("ICARUS_TEST_DROP_WAKEUPS")
        .env_remove("ICARUS_TEST_ARMED")
        .envs(env.iter().copied())
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    cmd.spawn().unwrap()
}
pub fn start(home: &Path, request: &Value, env: &[(&str, &str)]) -> Child {
    let mut child = child(home, "api", env);
    serde_json::to_writer(child.stdin.take().unwrap(), request).unwrap();
    child
}
pub fn api(home: &Path, request: &Value, env: &[(&str, &str)]) -> Output {
    start(home, request, env).wait_with_output().unwrap()
}
pub fn response(output: Output) -> Value {
    serde_json::from_slice(&output.stdout).unwrap_or_else(|e| {
        panic!(
            "{e}: {} {}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        )
    })
}
pub fn hook(
    h: &Harness,
    actor: &Actor,
    provider: &str,
    event: Value,
    crash: Option<&str>,
) -> Output {
    let mut env = vec![
        ("ICARUS_LAUNCH", actor.launch.as_str()),
        ("ICARUS_PROVIDER", provider),
    ];
    if let Some(point) = crash {
        env.push(("ICARUS_TEST_CRASH", point));
    }
    let mut child = child(h.dir.path(), "hook", &env);
    child
        .stdin
        .take()
        .unwrap()
        .write_all(event.to_string().as_bytes())
        .unwrap();
    child.wait_with_output().unwrap()
}
