mod json;
mod tap;

use json::Value;

fn input(args: &[String]) -> Result<String, String> {
    if args.len() != 2 {
        return Err("expected one JSON input".into());
    }
    let payload = json::parse(&args[1]).map_err(|e| format!("invalid input JSON: {}", e))?;
    let members = match &payload {
        Value::Object(members) => members,
        _ => return Err("invalid input JSON: expected an object".into()),
    };
    if members.iter().any(|(k, _)| k != "status") || members.is_empty() {
        return Err("expected only status".into());
    }
    match payload.get("status") {
        Some(Value::String(s)) if s == "open" || s == "done" => Ok(s.clone()),
        _ => Err("expected status open or done".into()),
    }
}

fn field<'a>(row: &'a Value, key: &str) -> Result<Option<&'a str>, String> {
    match row.get(key) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(s)) => Ok(Some(s)),
        Some(_) => Err("invalid task array".into()),
    }
}

fn summarize(client: &mut tap::Client, status: &str) -> Result<String, String> {
    let contents = client.read("examples/languages/fixtures/tasks.json").map_err(|e| e.to_string())?;
    let rows = match json::parse(&contents) {
        Ok(Value::Array(rows)) => rows,
        _ => return Err("invalid task array".into()),
    };
    let mut items = Vec::new();
    for row in &rows {
        if !matches!(row, Value::Object(_) | Value::Null) {
            return Err("invalid task array".into());
        }
        let (id, title, row_status) = (field(row, "id")?, field(row, "title")?, field(row, "status")?);
        match (id, title, row_status) {
            (Some(id), Some(title), Some(s)) if s == "open" || s == "done" => {
                if s == status {
                    items.push(format!("{{\"id\":{},\"title\":{}}}", json::quote(id), json::quote(title)));
                }
            }
            _ => return Err("invalid task row".into()),
        }
    }
    Ok(format!(
        "{{\"status\":{},\"count\":{},\"items\":[{}]}}",
        json::quote(status),
        items.len(),
        items.join(",")
    ))
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let mut client = tap::Client::new();
    let result = input(&args).and_then(|status| summarize(&mut client, &status));
    let sent = match result {
        Ok(stdout) => client.finish(&stdout, "", 0),
        Err(stderr) => client.finish("", &stderr, 1),
    };
    if sent.is_err() {
        // A broken protocol stream cannot be repaired with another frame.
        std::process::exit(1);
    }
}
