// api tests against a mock orchestrator: request shape, auth headers, error
// envelope handling, pagination walking, and the sse event stream.

use fuse::{
    Arch, Client, CreateRequest, Error, ExecRequest, ForkOptions, ListEnvironmentsOptions, Spec,
    ToolResultBlock,
};
use serde_json::json;
use wiremock::matchers::{body_partial_json, header, method, path, query_param};
use wiremock::{Mock, MockServer, ResponseTemplate};

fn env_body(id: &str, state: &str) -> serde_json::Value {
    json!({
        "id": id,
        "state": state,
        "task_id": "t-1",
        "url": format!("http://10.0.0.5/{id}"),
        "spec": {"cpus": 1},
        "created_at": "2026-01-02T03:04:05Z",
        "updated_at": "2026-01-02T03:04:06Z",
    })
}

async fn client(server: &MockServer) -> Client {
    Client::new(server.uri(), "secret").unwrap()
}

#[test]
fn builder_rejects_bad_base_urls() {
    assert!(matches!(
        Client::new("", "t"),
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        Client::new("not a url", "t"),
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        Client::new("mailto:nope@example.com", "t"),
        Err(Error::InvalidRequest(_))
    ));
    assert!(Client::new("http://127.0.0.1:8080", "").is_ok());
}

#[tokio::test]
async fn create_sends_auth_and_body() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments"))
        .and(header("authorization", "Bearer secret"))
        .and(header(
            "user-agent",
            concat!("fuse-rust/", env!("CARGO_PKG_VERSION")),
        ))
        .and(body_partial_json(json!({
            "task_id": "t-1",
            "spec": {"cpus": 2, "ram_mb": 2048},
        })))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-1", "provisioning")))
        .expect(1)
        .mount(&server)
        .await;

    let env = client(&server)
        .await
        .environments()
        .create(CreateRequest::new("t-1").spec(Spec::new().cpus(2).ram_mb(2048)))
        .await
        .unwrap();
    assert_eq!(env.id, "vm-1");
    assert_eq!(env.state, "provisioning".into());
}

#[tokio::test]
async fn empty_token_sends_no_auth_header() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-1"))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-1", "running")))
        .mount(&server)
        .await;

    let client = Client::new(server.uri(), "").unwrap();
    client.environments().get("vm-1").await.unwrap();

    let requests = server.received_requests().await.unwrap();
    assert!(requests[0].headers.get("authorization").is_none());
}

#[tokio::test]
async fn empty_ids_are_rejected_client_side() {
    let server = MockServer::start().await;
    let client = client(&server).await;
    assert!(matches!(
        client.environments().get("").await,
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        client.snapshots().delete("").await,
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        client.hosts().cordon("").await,
        Err(Error::InvalidRequest(_))
    ));
    assert!(server.received_requests().await.unwrap().is_empty());
}

#[tokio::test]
async fn api_error_envelope_is_parsed() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-x"))
        .respond_with(
            ResponseTemplate::new(404)
                .insert_header("x-request-id", "req-9")
                .set_body_json(json!({
                    "error": {
                        "code": "not_found",
                        "message": "environment not found",
                        "details": {"id": "vm-x"},
                    }
                })),
        )
        .mount(&server)
        .await;

    let err = client(&server)
        .await
        .environments()
        .get("vm-x")
        .await
        .unwrap_err();
    assert!(err.is_not_found());
    assert_eq!(err.status(), Some(404));
    let api = err.api().unwrap();
    assert_eq!(api.message, "environment not found");
    assert_eq!(api.request_id.as_deref(), Some("req-9"));
    assert_eq!(api.details.get("id").map(String::as_str), Some("vm-x"));
    assert!(err.to_string().contains("status=404"));
}

#[tokio::test]
async fn non_envelope_error_falls_back_to_status_text() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-x"))
        .respond_with(ResponseTemplate::new(502).set_body_string("<html>bad gateway</html>"))
        .mount(&server)
        .await;

    let err = client(&server)
        .await
        .environments()
        .get("vm-x")
        .await
        .unwrap_err();
    let api = err.api().unwrap();
    assert_eq!(api.status, 502);
    assert_eq!(api.code, None);
    assert_eq!(api.message, "bad gateway");
    assert_eq!(api.body, b"<html>bad gateway</html>");
}

#[tokio::test]
async fn list_walks_every_page() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/environments"))
        .and(query_param("cursor", "c1"))
        .and(query_param("limit", "200"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "environments": [env_body("vm-2", "running")],
        })))
        .expect(1)
        .mount(&server)
        .await;
    Mock::given(method("GET"))
        .and(path("/v1/environments"))
        .and(query_param("limit", "200"))
        .and(query_param("state", "running"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "environments": [env_body("vm-1", "running")],
            "next_cursor": "c1",
        })))
        .expect(1)
        .mount(&server)
        .await;

    let environments = client(&server)
        .await
        .environments()
        .list(ListEnvironmentsOptions::new().state("running".into()))
        .await
        .unwrap();
    assert_eq!(
        environments
            .iter()
            .map(|e| e.id.as_str())
            .collect::<Vec<_>>(),
        ["vm-1", "vm-2"]
    );
}

#[tokio::test]
async fn fork_posts_options_body() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "fork"))
        .and(body_partial_json(json!({"reuse_snapshot_id": "snap-1"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(env_body("vm-2", "provisioning")))
        .mount(&server)
        .await;

    let forked = client(&server)
        .await
        .environments()
        .fork("vm-1", ForkOptions::new().reuse_snapshot_id("snap-1"))
        .await
        .unwrap();
    assert_eq!(forked.id, "vm-2");
}

#[tokio::test]
async fn exec_decodes_result_and_rejects_empty_commands() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1"))
        .and(query_param("action", "exec"))
        .and(body_partial_json(json!({"cmd": ["false"]})))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "exit_code": 1,
            "stdout": "",
            "stderr": "boom",
        })))
        .mount(&server)
        .await;

    let client = client(&server).await;
    let result = client
        .environments()
        .exec("vm-1", ExecRequest::cmd(["false"]))
        .await
        .unwrap();
    assert!(!result.success());
    assert_eq!(result.stderr, "boom");

    let empty: [&str; 0] = [];
    assert!(matches!(
        client
            .environments()
            .exec("vm-1", ExecRequest::cmd(empty))
            .await,
        Err(Error::InvalidRequest(_))
    ));
    assert!(matches!(
        client
            .environments()
            .exec("vm-1", ExecRequest::shell(""))
            .await,
        Err(Error::InvalidRequest(_))
    ));
}

#[tokio::test]
async fn events_stream_parses_sse_and_stops_at_terminal_state() {
    let server = MockServer::start().await;
    let body = concat!(
        ": keepalive\n",
        "data: {\"id\":\"1\",\"vm_id\":\"vm-1\",\"state\":\"provisioning\"}\n",
        "\n",
        "data: {\"event\":\"step\",\"vm_id\":\"vm-1\",\"state\":\"\",\"index\":1,\"total\":2,\"key\":\"apt\",\"cached\":true}\n",
        "\n",
        "data: {\"vm_id\":\"vm-1\",\"state\":\"destroyed\"}\n",
        "\n",
        "data: {\"vm_id\":\"vm-1\",\"state\":\"running\"}\n",
        "\n",
    );
    Mock::given(method("GET"))
        .and(path("/v1/environments/vm-1/events"))
        .and(header("accept", "text/event-stream"))
        .respond_with(ResponseTemplate::new(200).set_body_raw(body, "text/event-stream"))
        .mount(&server)
        .await;

    let mut events = client(&server)
        .await
        .environments()
        .events("vm-1")
        .await
        .unwrap();

    let first = events.next().await.unwrap().unwrap();
    assert_eq!(first.state, Some("provisioning".into()));

    let step = events.next().await.unwrap().unwrap();
    assert!(!step.is_state());
    assert!(step.cached);

    let terminal = events.next().await.unwrap().unwrap();
    assert!(terminal.is_terminal());

    // the terminal event closes the stream; the trailing "running" event
    // must not be delivered.
    assert!(events.next().await.is_none());
    assert!(events.next().await.is_none());
}

#[tokio::test]
async fn computer_tool_result_maps_blocks_and_tool_errors() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-1/computer"))
        .and(body_partial_json(json!({"action": "screenshot"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"screenshot": "cGpn"})))
        .mount(&server)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/environments/vm-2/computer"))
        .respond_with(ResponseTemplate::new(503).set_body_json(json!({
            "error": {"code": "unavailable", "message": "display is not up"},
        })))
        .mount(&server)
        .await;

    let client = client(&server).await;

    let ok = client
        .environments()
        .computer_tool_result("vm-1", &json!({"action": "screenshot"}))
        .await
        .unwrap();
    assert!(!ok.is_error);
    assert!(matches!(
        &ok.content[0],
        ToolResultBlock::Image { source } if source.data == "cGpn"
    ));

    // the environment refused the action: that goes back to the model as an
    // error tool_result, not up the stack as an Err.
    let refused = client
        .environments()
        .computer_tool_result("vm-2", &json!({"action": "screenshot"}))
        .await
        .unwrap();
    assert!(refused.is_error);
    assert!(matches!(
        &refused.content[0],
        ToolResultBlock::Text { text } if text == "display is not up"
    ));

    let no_action = client
        .environments()
        .computer_tool_result("vm-1", &json!({"coordinate": [1, 2]}))
        .await
        .unwrap();
    assert!(no_action.is_error);

    assert!(matches!(
        client
            .environments()
            .computer_tool_result("vm-1", &json!("not an object"))
            .await,
        Err(Error::InvalidRequest(_))
    ));
}

#[tokio::test]
async fn snapshot_resolve_reports_miss_as_none() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/snapshots/resolve"))
        .and(query_param("layer_key", "abc"))
        .and(query_param("arch", "amd64"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"found": false})))
        .mount(&server)
        .await;

    let resolved = client(&server)
        .await
        .snapshots()
        .resolve("abc", Arch::Amd64)
        .await
        .unwrap();
    assert!(resolved.is_none());
}

#[tokio::test]
async fn api_keys_round_trip() {
    let server = MockServer::start().await;
    Mock::given(method("POST"))
        .and(path("/v1/api-keys"))
        .and(body_partial_json(json!({"label": "ci"})))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({
            "id": "key-1",
            "label": "ci",
            "created_at": "2026-01-02T03:04:05Z",
            "key": "fuse_sk_live",
        })))
        .mount(&server)
        .await;
    Mock::given(method("DELETE"))
        .and(path("/v1/api-keys/key-1"))
        .respond_with(ResponseTemplate::new(204))
        .mount(&server)
        .await;

    let client = client(&server).await;
    let created = client.api_keys().create("ci").await.unwrap();
    assert_eq!(created.key, "fuse_sk_live");
    assert_eq!(created.api_key.id, "key-1");
    client.api_keys().revoke("key-1").await.unwrap();
}

#[tokio::test]
async fn version_distinguishes_wrong_service() {
    let server = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/version"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("server", "fuse-orchestrator/0.24.0")
                .set_body_json(json!({"service": "fuse-orchestrator", "version": "0.24.0"})),
        )
        .mount(&server)
        .await;

    let info = client(&server).await.version().await.unwrap();
    assert!(info.is_orchestrator());
    assert_eq!(info.version, "0.24.0");
    assert_eq!(info.server_header, "fuse-orchestrator/0.24.0");

    // a reachable-but-wrong service is a report, not an error.
    let wrong = MockServer::start().await;
    Mock::given(method("GET"))
        .and(path("/v1/version"))
        .respond_with(
            ResponseTemplate::new(404)
                .insert_header("server", "fc-agent/0.1")
                .set_body_string("not json"),
        )
        .mount(&wrong)
        .await;
    let info = Client::new(wrong.uri(), "")
        .unwrap()
        .version()
        .await
        .unwrap();
    assert!(!info.is_orchestrator());
    assert_eq!(info.server_header, "fc-agent/0.1");
    assert_eq!(info.status, 404);
}
