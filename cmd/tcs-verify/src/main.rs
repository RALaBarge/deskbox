use serde::Deserialize;
use schemars::schema_for;
use std::fs;
use std::process::Command;

#[derive(Deserialize)]
struct ExampleOutput {
    result: String,
}

fn main() {
    let schema = schema_for!(ExampleOutput);
    let output = r#"{"result":"success"}"#;
    let parsed: ExampleOutput = serde_json::from_str(output).unwrap();
    println!("Valid output: {:?}", parsed);
}