pub mod mcp;
pub mod store;
pub mod types;
pub mod wake;

/// A causal barrier for subprocess tests, used once per selected process.
/// The launcher excludes these environment variables from native sessions.
pub fn barrier_point(point: &str) -> anyhow::Result<()> {
    use std::{
        path::PathBuf,
        sync::atomic::{AtomicBool, Ordering},
        time::{Duration, Instant},
    };
    static ENTERED: AtomicBool = AtomicBool::new(false);
    if std::env::var("ICARUS_TEST_PAUSE").as_deref() != Ok(point)
        || ENTERED.swap(true, Ordering::Relaxed)
    {
        return Ok(());
    }
    let directory = PathBuf::from(std::env::var("ICARUS_TEST_BARRIER_DIR")?);
    std::fs::write(directory.join(format!("{point}.ready")), b"ready")?;
    let release = directory.join(format!("{point}.release"));
    let deadline = Instant::now() + Duration::from_secs(15);
    while !release.exists() {
        anyhow::ensure!(Instant::now() < deadline, "test barrier timed out: {point}");
        std::thread::sleep(Duration::from_millis(2));
    }
    Ok(())
}

/// Only explicitly selected fault-test processes exit here. Recovery must use
/// the same database and operation ID, including after an ambiguous commit.
pub fn crash_point(point: &str) {
    if std::env::var("ICARUS_TEST_CRASH").as_deref() == Ok(point) {
        std::process::exit(86);
    }
}
