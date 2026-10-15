use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::fmt;

macro_rules! identifier {
    ($name:ident) => {
        #[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Serialize, Deserialize)]
        #[serde(try_from = "String", into = "String")]
        pub struct $name(String);
        impl $name {
            pub fn new() -> Self {
                Self(uuid::Uuid::new_v4().to_string())
            }
            pub fn as_str(&self) -> &str {
                &self.0
            }
        }
        impl Default for $name {
            fn default() -> Self {
                Self::new()
            }
        }
        impl TryFrom<String> for $name {
            type Error = anyhow::Error;
            fn try_from(s: String) -> Result<Self> {
                ensure!(
                    !s.is_empty()
                        && s.len() <= 128
                        && s.bytes()
                            .all(|c| c.is_ascii_alphanumeric() || b"-_.:".contains(&c)),
                    "invalid {}",
                    stringify!($name)
                );
                Ok(Self(s))
            }
        }
        impl From<$name> for String {
            fn from(v: $name) -> Self {
                v.0
            }
        }
        impl fmt::Display for $name {
            fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                self.0.fmt(f)
            }
        }
    };
}
identifier!(GroupId);
identifier!(MemberId);
identifier!(LaunchId);
identifier!(TaskId);
identifier!(MessageId);
identifier!(OperationId);

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "u64", into = "u64")]
pub struct Revision(pub u64);
impl TryFrom<u64> for Revision {
    type Error = anyhow::Error;
    fn try_from(n: u64) -> Result<Self> {
        ensure!(n < i64::MAX as u64, "revision out of range");
        Ok(Self(n))
    }
}
impl From<Revision> for u64 {
    fn from(r: Revision) -> Self {
        r.0
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Provider {
    Codex,
    Claude,
}
impl Provider {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Codex => "codex",
            Self::Claude => "claude",
        }
    }
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(tag = "status", rename_all = "snake_case", deny_unknown_fields)]
pub enum TaskResult {
    Success { value: Value },
    Failure { value: Value },
    Cancelled { value: Value },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Request {
    pub version: u32,
    #[serde(default)]
    pub operation_id: Option<OperationId>,
    #[serde(default)]
    pub member: Option<MemberId>,
    #[serde(default)]
    pub launch: Option<LaunchId>,
    pub command: Command,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "method", rename_all = "snake_case", deny_unknown_fields)]
pub enum Command {
    LaunchStart {
        provider: Provider,
        instance: String,
    },
    Bind {
        session: String,
        agent: Option<String>,
        parent_agent: Option<String>,
    },
    Members,
    Send {
        to: MemberId,
        body: String,
    },
    Inbox {
        limit: Option<u32>,
    },
    Ack {
        through: MessageId,
    },
    TaskCreate {
        owner: MemberId,
        description: String,
    },
    TaskRead {
        task: TaskId,
    },
    TaskFinish {
        task: TaskId,
        result: TaskResult,
    },
    CellRead {
        key: String,
    },
    CellCas {
        key: String,
        expected: Revision,
        value: Value,
    },
    WaitAny {
        messages: bool,
        tasks: Vec<TaskId>,
        cells: Vec<CellWatch>,
        timeout_ms: u64,
    },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CellWatch {
    pub key: String,
    pub revision: Revision,
}

impl Command {
    pub fn is_mutation(&self) -> bool {
        !matches!(
            self,
            Self::Members | Self::TaskRead { .. } | Self::CellRead { .. } | Self::WaitAny { .. }
        )
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Response {
    pub version: u32,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}
impl Response {
    pub fn from_result(r: Result<Value>) -> Self {
        match r {
            Ok(v) => Self {
                version: 1,
                ok: true,
                result: Some(v),
                error: None,
            },
            Err(e) => Self {
                version: 1,
                ok: false,
                result: None,
                error: Some(format!("{e:#}")),
            },
        }
    }
}
