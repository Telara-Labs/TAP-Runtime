//! A small JSON reader and string encoder, so the example builds with the
//! Rust standard library alone and no downloaded crates.

#[derive(Debug, Clone, PartialEq)]
pub enum Value {
    Null,
    Bool(bool),
    Number(f64),
    String(String),
    Array(Vec<Value>),
    Object(Vec<(String, Value)>),
}

impl Value {
    /// Returns the member with this key. A repeated key keeps its last value.
    pub fn get(&self, key: &str) -> Option<&Value> {
        match self {
            Value::Object(members) => members.iter().rev().find(|(k, _)| k == key).map(|(_, v)| v),
            _ => None,
        }
    }
}

pub fn parse(text: &str) -> Result<Value, String> {
    let mut p = Parser { b: text.as_bytes(), i: 0 };
    let v = p.value(0)?;
    p.ws();
    if p.i != p.b.len() {
        return Err(format!("unexpected data at byte {}", p.i));
    }
    Ok(v)
}

struct Parser<'a> {
    b: &'a [u8],
    i: usize,
}

impl Parser<'_> {
    fn ws(&mut self) {
        while self.i < self.b.len() && matches!(self.b[self.i], b' ' | b'\t' | b'\n' | b'\r') {
            self.i += 1;
        }
    }

    fn eat(&mut self, c: u8) -> bool {
        self.ws();
        if self.i < self.b.len() && self.b[self.i] == c {
            self.i += 1;
            return true;
        }
        false
    }

    fn literal(&mut self, word: &str, v: Value) -> Result<Value, String> {
        if self.b[self.i..].starts_with(word.as_bytes()) {
            self.i += word.len();
            return Ok(v);
        }
        Err(format!("invalid literal at byte {}", self.i))
    }

    fn value(&mut self, depth: usize) -> Result<Value, String> {
        if depth > 128 {
            return Err("nesting too deep".into());
        }
        self.ws();
        match self.b.get(self.i) {
            None => Err("unexpected end of JSON".into()),
            Some(b'n') => self.literal("null", Value::Null),
            Some(b't') => self.literal("true", Value::Bool(true)),
            Some(b'f') => self.literal("false", Value::Bool(false)),
            Some(b'"') => Ok(Value::String(self.string()?)),
            Some(b'[') => {
                self.i += 1;
                let mut items = Vec::new();
                if self.eat(b']') {
                    return Ok(Value::Array(items));
                }
                loop {
                    items.push(self.value(depth + 1)?);
                    if self.eat(b']') {
                        return Ok(Value::Array(items));
                    }
                    if !self.eat(b',') {
                        return Err(format!("expected , or ] at byte {}", self.i));
                    }
                }
            }
            Some(b'{') => {
                self.i += 1;
                let mut members = Vec::new();
                if self.eat(b'}') {
                    return Ok(Value::Object(members));
                }
                loop {
                    self.ws();
                    if self.b.get(self.i) != Some(&b'"') {
                        return Err(format!("expected a key at byte {}", self.i));
                    }
                    let key = self.string()?;
                    if !self.eat(b':') {
                        return Err(format!("expected : at byte {}", self.i));
                    }
                    members.push((key, self.value(depth + 1)?));
                    if self.eat(b'}') {
                        return Ok(Value::Object(members));
                    }
                    if !self.eat(b',') {
                        return Err(format!("expected , or }} at byte {}", self.i));
                    }
                }
            }
            Some(_) => self.number(),
        }
    }

    fn number(&mut self) -> Result<Value, String> {
        let start = self.i;
        while self.i < self.b.len() && matches!(self.b[self.i], b'-' | b'+' | b'.' | b'e' | b'E' | b'0'..=b'9') {
            self.i += 1;
        }
        let text = std::str::from_utf8(&self.b[start..self.i]).unwrap_or("");
        text.parse::<f64>()
            .ok()
            .filter(|n| n.is_finite() && !text.starts_with('+') && !text.starts_with('.'))
            .map(Value::Number)
            .ok_or_else(|| format!("invalid value at byte {}", start))
    }

    fn hex4(&mut self) -> Result<u32, String> {
        let digits = self.b.get(self.i..self.i + 4).ok_or("truncated escape")?;
        let text = std::str::from_utf8(digits).map_err(|_| "invalid escape")?;
        self.i += 4;
        u32::from_str_radix(text, 16).map_err(|_| "invalid escape".into())
    }

    fn string(&mut self) -> Result<String, String> {
        self.i += 1; // opening quote
        let mut out = String::new();
        loop {
            let start = self.i;
            while self.i < self.b.len() && self.b[self.i] != b'"' && self.b[self.i] != b'\\' && self.b[self.i] >= 0x20 {
                self.i += 1;
            }
            out.push_str(std::str::from_utf8(&self.b[start..self.i]).map_err(|_| "invalid UTF-8")?);
            match self.b.get(self.i) {
                Some(b'"') => {
                    self.i += 1;
                    return Ok(out);
                }
                Some(b'\\') => {
                    self.i += 1;
                    let c = *self.b.get(self.i).ok_or("truncated escape")?;
                    self.i += 1;
                    match c {
                        b'"' => out.push('"'),
                        b'\\' => out.push('\\'),
                        b'/' => out.push('/'),
                        b'b' => out.push('\u{8}'),
                        b'f' => out.push('\u{c}'),
                        b'n' => out.push('\n'),
                        b'r' => out.push('\r'),
                        b't' => out.push('\t'),
                        b'u' => {
                            let mut code = self.hex4()?;
                            if (0xd800..0xdc00).contains(&code) && self.b[self.i..].starts_with(b"\\u") {
                                self.i += 2;
                                let low = self.hex4()?;
                                if !(0xdc00..0xe000).contains(&low) {
                                    return Err("invalid surrogate pair".into());
                                }
                                code = 0x10000 + ((code - 0xd800) << 10) + (low - 0xdc00);
                            }
                            out.push(char::from_u32(code).ok_or("invalid code point")?);
                        }
                        _ => return Err("invalid escape".into()),
                    }
                }
                _ => return Err("unterminated string".into()),
            }
        }
    }
}

/// Encodes s as a JSON string literal.
pub fn quote(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}
