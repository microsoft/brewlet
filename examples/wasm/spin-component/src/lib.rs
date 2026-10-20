use spin_sdk::http::{IntoResponse, Request, Response};
use spin_sdk::http_service;

#[http_service]
async fn handle(request: Request) -> anyhow::Result<impl IntoResponse> {
    let dependency = std::fs::read_to_string("/app/dependencies/message.txt")?;
    let hidden = std::fs::read("/etc/runtime-sentinel").is_err();
    let cache_hidden = std::fs::read_dir("/cache").is_err();
    let readonly = std::fs::write("/app/dependencies/forbidden", b"must not write").is_err();
    let visible = std::env::var("VISIBLE")?;
    let host_only = match std::env::var("HOST_ONLY") {
        Ok(value) => value,
        Err(std::env::VarError::NotPresent) => String::new(),
        Err(error) => return Err(error.into()),
    };
    anyhow::ensure!(
        hidden && cache_hidden && readonly && host_only.is_empty() && visible == "explicit",
        "guest capability boundary violated"
    );
    let report = serde_json::json!({
        "message": "Hello from Wasm",
        "dependency": dependency,
        "visible": visible,
        "hostOnly": host_only,
        "runtimeFileHidden": hidden,
        "engineCacheHidden": cache_hidden,
        "dependencyWriteDenied": readonly,
        "pathInfo": request.uri().path()
    });
    Ok(Response::builder()
        .status(200)
        .header("content-type", "application/json")
        .body(report.to_string())?)
}
