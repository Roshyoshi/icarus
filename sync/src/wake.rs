use crate::{
    barrier_point,
    store::Store,
    types::{Command, Request},
};
use anyhow::{Result, ensure};
use serde_json::{Value, json};
use std::{
    fs, io,
    os::unix::{fs::PermissionsExt, net::UnixDatagram},
    path::{Path, PathBuf},
    sync::atomic::{AtomicBool, Ordering},
    time::{Duration, Instant},
};

pub struct Waiter {
    socket: UnixDatagram,
    socket_path: PathBuf,
    registry_path: PathBuf,
}

impl Waiter {
    pub fn arm(directory: &Path) -> Result<Self> {
        let name = uuid::Uuid::new_v4().to_string();
        // sockaddr_un is only 104 bytes on macOS. ICARUS_HOME may be much longer.
        let socket_path = PathBuf::from(format!("/tmp/icarus-{name}.sock"));
        let socket = UnixDatagram::bind(&socket_path)?;
        let registry = directory.join("wake");
        let registry_path = registry.join(&name);
        let waiter = Self {
            socket,
            socket_path,
            registry_path,
        };
        fs::set_permissions(&waiter.socket_path, fs::Permissions::from_mode(0o600))?;
        fs::create_dir_all(&registry)?;
        fs::write(
            &waiter.registry_path,
            waiter.socket_path.to_string_lossy().as_bytes(),
        )?;
        Ok(waiter)
    }

    pub fn sleep(&self, duration: Duration) -> Result<()> {
        self.socket
            .set_read_timeout(Some(duration.max(Duration::from_millis(1))))?;
        match self.socket.recv(&mut [0; 1]) {
            Ok(_) => Ok(()),
            Err(e)
                if matches!(
                    e.kind(),
                    io::ErrorKind::TimedOut
                        | io::ErrorKind::WouldBlock
                        | io::ErrorKind::Interrupted
                ) =>
            {
                Ok(())
            }
            Err(e) => Err(e.into()),
        }
    }
}
impl Drop for Waiter {
    fn drop(&mut self) {
        let _ = fs::remove_file(&self.registry_path);
        let _ = fs::remove_file(&self.socket_path);
    }
}

pub fn notify(directory: &Path) {
    let dropped = std::env::var_os("ICARUS_TEST_DROP_WAKEUPS").is_some();
    if dropped {
        notification_report(true, 0, 0, 0);
        return;
    }
    let Ok(entries) = fs::read_dir(directory.join("wake")) else {
        notification_report(false, 0, 0, 0);
        return;
    };
    let Ok(socket) = UnixDatagram::unbound() else {
        notification_report(false, 0, 0, 0);
        return;
    };
    let (mut targets, mut sent, mut failed) = (0, 0, 0);
    let _ = socket.set_nonblocking(true);
    for entry in entries.flatten() {
        let Ok(path) = fs::read_to_string(entry.path()) else {
            continue;
        };
        let expected = format!("/tmp/icarus-{}.sock", entry.file_name().to_string_lossy());
        if path != expected {
            continue;
        }
        targets += 1;
        match socket.send_to(&[1], &path) {
            Ok(_) => sent += 1,
            Err(e) => {
                failed += 1;
                if matches!(
                    e.kind(),
                    io::ErrorKind::NotFound | io::ErrorKind::ConnectionRefused
                ) {
                    let _ = fs::remove_file(entry.path());
                    let _ = fs::remove_file(path);
                }
            }
        }
    }
    notification_report(false, targets, sent, failed);
}

fn notification_report(dropped: bool, targets: u64, sent: u64, failed: u64) {
    if let Some(path) = std::env::var_os("ICARUS_TEST_NOTIFY_REPORT") {
        // Test telemetry cannot change the result of an already committed
        // operation. The test requires this file and rejects a missing report.
        let _ = fs::write(
            path,
            json!({"dropped":dropped,"targets":targets,
            "sent":sent,"failed":failed})
            .to_string(),
        );
    }
}

pub fn wait_any(store: &mut Store, request: &Request, cancelled: &AtomicBool) -> Result<Value> {
    ensure!(request.version == 1, "unsupported protocol version");
    let Command::WaitAny {
        messages,
        tasks,
        cells,
        timeout_ms,
    } = &request.command
    else {
        anyhow::bail!("expected wait_any");
    };
    ensure!(
        *messages || !tasks.is_empty() || !cells.is_empty(),
        "empty wait set"
    );
    ensure!(
        tasks.len() + cells.len() <= 64 && *timeout_ms <= 86_400_000,
        "wait bounds exceeded"
    );
    // Notifications are an optimization. Sandboxes may deny Unix sockets;
    // polling still has the same correctness and cancellation contract.
    barrier_point("before_arm")?;
    let waiter = Waiter::arm(&store.directory).ok();
    barrier_point("after_arm")?;
    let deadline = Instant::now() + Duration::from_millis(*timeout_ms);
    // Used by multiprocess tests as a causal barrier, before the first read.
    if let Some(path) = std::env::var_os("ICARUS_TEST_ARMED") {
        fs::write(path, b"armed")?;
    }
    loop {
        if cancelled.load(Ordering::Acquire) {
            return Ok(json!({"status":"cancelled"}));
        }
        let ready = store.ready(request)?;
        if ready["messages"] == true
            || !ready["tasks"].as_array().unwrap().is_empty()
            || !ready["cells"].as_array().unwrap().is_empty()
        {
            return Ok(json!({"status":"ready","ready":ready}));
        }
        barrier_point("after_empty_read")?;
        let remaining = deadline.saturating_duration_since(Instant::now());
        if remaining.is_zero() {
            return Ok(json!({"status":"timeout"}));
        }
        let pause = remaining.min(Duration::from_secs(1));
        if let Some(waiter) = &waiter {
            waiter.sleep(pause)?;
        } else {
            std::thread::sleep(pause);
        }
    }
}
