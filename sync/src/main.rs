use anyhow::{Context, Result, bail, ensure};
use icarus_sync::{store::Store, types::*, wake};
use std::{
    io::{self, Read, Write},
    path::PathBuf,
    sync::atomic::AtomicBool,
};

fn main() {
    if let Err(e) = run() {
        eprintln!("icarus-sync: {e:#}");
        std::process::exit(1);
    }
}

fn run() -> Result<()> {
    let mut home = std::env::var_os("ICARUS_HOME").map(PathBuf::from);
    let mut group = std::env::var("ICARUS_GROUP").ok();
    let mut mode = None;
    let mut args = std::env::args().skip(1);
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--home" => home = Some(args.next().context("--home requires path")?.into()),
            "--group" => group = Some(args.next().context("--group requires ID")?),
            "--help" | "-h" => {
                println!(
                    "icarus-sync [--home PATH] [--group GROUP] api|history\nicarus-sync version\nJSON protocol v1; api reads one request from stdin. See docs/protocol.md."
                );
                return Ok(());
            }
            _ if mode.is_none() => mode = Some(arg),
            _ => bail!("unexpected argument: {arg}"),
        }
    }
    let mode = mode.context("expected api, history, or version")?;
    if mode == "version" {
        let sqlite_source_id: String = rusqlite::Connection::open_in_memory()?.query_row(
            "SELECT sqlite_source_id()",
            [],
            |row| row.get(0),
        )?;
        println!(
            "{}",
            serde_json::json!({"protocol":1,"version":env!("CARGO_PKG_VERSION"),
                "sqlite_version":rusqlite::version(), "sqlite_source_id":sqlite_source_id})
        );
        return Ok(());
    }
    let home = home.context("ICARUS_HOME or --home required")?;
    let group = GroupId::try_from(group.context("ICARUS_GROUP or --group required")?)?;
    let mut store = Store::open(&home, &group)?;
    if mode == "history" {
        println!("{}", store.history()?);
        return Ok(());
    }
    let mut input = String::new();
    io::stdin().take(262145).read_to_string(&mut input)?;
    ensure!(input.len() <= 262144, "input exceeds 256 KiB");
    match mode.as_str() {
        "api" => {
            let result = serde_json::from_str::<Request>(&input)
                .map_err(Into::into)
                .and_then(|req| {
                    if matches!(req.command, Command::WaitAny { .. }) {
                        wake::wait_any(&mut store, &req, &AtomicBool::new(false))
                    } else {
                        store.execute(&req)
                    }
                });
            let response = Response::from_result(result);
            println!("{}", serde_json::to_string(&response)?);
            io::stdout().flush()?;
            if !response.ok {
                std::process::exit(1);
            }
        }
        _ => bail!("unknown command: {mode}"),
    }
    Ok(())
}
