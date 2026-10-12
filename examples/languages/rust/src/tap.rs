//! Minimal JSON-line broker client for a compiled Rust primitive.
//!
//! The same client works when the program is built as a WASI Preview 1
//! module (wasm32-wasip1) or a WASI Preview 2 component (wasm32-wasip2): both
//! speak TAP's protocol over standard input and output. It opens no files and
//! reads no environment; the runner answers each request.

use crate::json::{self, Value};
use std::io::{BufRead, Write};

pub struct Client {
    next: u64,
}

/// Why a broker request did not produce a usable result.
#[derive(Debug)]
pub enum Error {
    Protocol(String),
    Refused(String),
    Violation(String),
    Unknown(String),
    Failed(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::Protocol(d) => write!(f, "protocol: {}", d),
            Error::Refused(d) => write!(f, "refused: {}", d),
            Error::Violation(d) => write!(f, "violation: {}", d),
            Error::Unknown(d) => write!(f, "unknown: {}", d),
            Error::Failed(d) => write!(f, "failed: {}", d),
        }
    }
}

fn text(v: Option<&Value>) -> String {
    match v {
        Some(Value::String(s)) => s.clone(),
        _ => String::new(),
    }
}

fn truthy(v: Option<&Value>) -> bool {
    matches!(v, Some(Value::Bool(true)))
}

impl Client {
    pub fn new() -> Client {
        Client { next: 0 }
    }

    fn send(&self, frame: &str) -> Result<(), Error> {
        let mut out = std::io::stdout().lock();
        out.write_all(frame.as_bytes())
            .and_then(|_| out.write_all(b"\n"))
            .and_then(|_| out.flush())
            .map_err(|e| Error::Protocol(format!("write broker frame: {}", e)))
    }

    /// Sends one request. fields is the JSON text of the method's fields
    /// without braces, for example `"path":"tasks.json"`.
    pub fn request(&mut self, method: &str, fields: &str) -> Result<Value, Error> {
        self.next += 1;
        let id = format!("r{}", self.next);
        let mut frame = format!("{{\"id\":{},\"method\":{}", json::quote(&id), json::quote(method));
        if !fields.is_empty() {
            frame.push(',');
            frame.push_str(fields);
        }
        frame.push('}');
        self.send(&frame)?;
        let mut line = String::new();
        std::io::stdin()
            .lock()
            .read_line(&mut line)
            .map_err(|e| Error::Protocol(format!("read broker response: {}", e)))?;
        let reply = json::parse(&line).map_err(|e| Error::Protocol(format!("decode broker response: {}", e)))?;
        if text(reply.get("id")) != id {
            return Err(Error::Protocol(format!("response ID does not match request ID {}", id)));
        }
        if truthy(reply.get("unknown")) {
            return Err(Error::Unknown(text(reply.get("refused"))));
        }
        let refused = text(reply.get("refused"));
        if !refused.is_empty() {
            return Err(Error::Refused(refused));
        }
        if truthy(reply.get("violation")) {
            return Err(Error::Violation(text(reply.get("stderr"))));
        }
        Ok(reply)
    }

    /// Reads one path the manifest declares with access: read.
    pub fn read(&mut self, path: &str) -> Result<String, Error> {
        if path.is_empty() {
            return Err(Error::Protocol("read path must not be empty".into()));
        }
        let reply = self.request("read", &format!("\"path\":{}", json::quote(path)))?;
        if let Some(Value::Number(exit)) = reply.get("exit") {
            if *exit != 0.0 {
                return Err(Error::Failed(format!("read failed: {}", text(reply.get("stderr")))));
            }
        }
        match reply.get("result") {
            Some(Value::String(s)) => Ok(s.clone()),
            None => Ok(String::new()),
            _ => Err(Error::Protocol("broker result is not a string".into())),
        }
    }

    /// Sends the final result. No request may follow it.
    pub fn finish(&self, stdout: &str, stderr: &str, exit: u32) -> Result<(), Error> {
        self.send(&format!(
            "{{\"method\":\"return\",\"stdout\":{},\"stderr\":{},\"exit\":{}}}",
            json::quote(stdout),
            json::quote(stderr),
            exit
        ))
    }
}
