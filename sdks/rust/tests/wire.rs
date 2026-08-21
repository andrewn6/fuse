// wire-shape tests: these pin the exact json this sdk puts on and reads off
// the wire, so a refactor cannot silently drift from the server's schema.

use fuse::{
    Arch, ComputerAction, CreateRequest, EnvironmentState, Event, EventKind, ExecRequest,
    ExposeSpec, HealthcheckHttp, HealthcheckSpec, MissReason, ScrollDirection, Snapshot,
    SnapshotKind, SnapshotRequest, Spec,
};
use serde_json::json;

#[test]
fn spec_serializes_only_set_fields() {
    let spec = Spec::new().cpus(2).ram_mb(2048).arch(Arch::Arm64);
    assert_eq!(
        serde_json::to_value(&spec).unwrap(),
        json!({"cpus": 2, "ram_mb": 2048, "arch": "arm64"})
    );
    assert_eq!(serde_json::to_value(Spec::new()).unwrap(), json!({}));
}

#[test]
fn create_request_wire_shape() {
    let request = CreateRequest::new("t-1")
        .spec(Spec::new().image("img").label("gpu", "true"))
        .secret("API_KEY", "hunter2")
        .expose(ExposeSpec::named(8080, "web"))
        .desktop(1280, 800)
        .file("/etc/motd", b"hello")
        .seed_snapshot_id("snap-1");
    assert_eq!(
        serde_json::to_value(&request).unwrap(),
        json!({
            "task_id": "t-1",
            "spec": {"image": "img", "labels": {"gpu": "true"}},
            "secrets": {"API_KEY": "hunter2"},
            "expose": [{"port": 8080, "as": "web"}],
            "desktop": {"width": 1280, "height": 800},
            "files": {"/etc/motd": "aGVsbG8="},
            "seed_snapshot_id": "snap-1",
        })
    );
}

#[test]
fn healthcheck_probe_is_exactly_one_of_http_and_exec() {
    let http = HealthcheckSpec::http(HealthcheckHttp::new(3000).path("/healthz"))
        .interval_seconds(5)
        .retries(3);
    assert_eq!(
        serde_json::to_value(&http).unwrap(),
        json!({
            "http": {"port": 3000, "path": "/healthz"},
            "interval_seconds": 5,
            "retries": 3,
        })
    );

    let exec = HealthcheckSpec::exec(["curl", "-f", "http://localhost:3000"]);
    assert_eq!(
        serde_json::to_value(&exec).unwrap(),
        json!({"exec": ["curl", "-f", "http://localhost:3000"]})
    );
}

#[test]
fn exec_request_serializes_cmd_xor_shell() {
    let cmd = ExecRequest::cmd(["ls", "-l"]);
    assert_eq!(
        serde_json::to_value(&cmd).unwrap(),
        json!({"cmd": ["ls", "-l"]})
    );

    let shell = ExecRequest::shell("dmesg | tail").timeout_ms(5000);
    assert_eq!(
        serde_json::to_value(&shell).unwrap(),
        json!({"shell": "dmesg | tail", "timeout_ms": 5000})
    );
}

#[test]
fn computer_action_wire_shape() {
    let scroll = ComputerAction::scroll(640, 400, ScrollDirection::Down, 3);
    assert_eq!(
        serde_json::to_value(&scroll).unwrap(),
        json!({
            "action": "scroll",
            "coordinate": [640, 400],
            "scroll_direction": "down",
            "scroll_amount": 3,
        })
    );

    let zoom = ComputerAction::new("zoom".into()).region(0, 0, 800, 600);
    assert_eq!(
        serde_json::to_value(&zoom).unwrap(),
        json!({"action": "zoom", "region": [0, 0, 800, 600]})
    );
}

#[test]
fn unknown_enum_values_pass_through() {
    let state: EnvironmentState = serde_json::from_value(json!("hibernating")).unwrap();
    assert_eq!(state, EnvironmentState::Other("hibernating".to_owned()));
    assert_eq!(serde_json::to_value(&state).unwrap(), json!("hibernating"));
    assert!(!state.is_terminal());

    assert_eq!(EnvironmentState::from("failed"), EnvironmentState::Failed);
    assert!(EnvironmentState::Failed.is_terminal());
    assert!(EnvironmentState::Running.is_settled());
    assert!(!EnvironmentState::Provisioning.is_settled());
}

#[test]
fn state_event_deserializes() {
    let event: Event = serde_json::from_value(json!({
        "id": "ev-1",
        "vm_id": "vm-1",
        "state": "running",
        "url": "http://10.0.0.5",
        "updated_at": "2026-01-02T03:04:05Z",
    }))
    .unwrap();
    // an absent kind means "state": older servers omit the field.
    assert_eq!(event.kind, EventKind::State);
    assert!(event.is_state());
    assert_eq!(event.state, Some(EnvironmentState::Running));
    assert!(!event.is_terminal());
}

#[test]
fn step_event_deserializes() {
    let event: Event = serde_json::from_value(json!({
        "event": "step",
        "vm_id": "vm-1",
        "state": "",
        "index": 2,
        "total": 5,
        "key": "apt-install",
        "cached": false,
        "miss_reason": "inputs-changed",
        "miss_detail": ["requirements.txt"],
        "duration_ms": 1200,
        "exit_code": 0,
    }))
    .unwrap();
    assert_eq!(event.kind, EventKind::Step);
    assert!(!event.is_state());
    // the empty state string means "no state", not Other("").
    assert_eq!(event.state, None);
    assert_eq!(event.miss_reason, Some(MissReason::InputsChanged));
    assert_eq!(event.index, 2);
}

#[test]
fn snapshot_request_omits_live_unless_asked() {
    // an ordinary create must not put "live": false on the wire: the flag is
    // opt-in on both ends, and an agent that does not read it should see a
    // request identical to the one every caller sent before live existed.
    assert_eq!(
        serde_json::to_value(SnapshotRequest::new().comment("nightly")).unwrap(),
        json!({"comment": "nightly"})
    );
    assert_eq!(
        serde_json::to_value(SnapshotRequest::new().live(true)).unwrap(),
        json!({"live": true})
    );
}

#[test]
fn snapshot_kind_decodes_and_survives_an_unknown_value() {
    let live: Snapshot = serde_json::from_value(json!({
        "id": "snap-1", "kind": "live", "created_at": "2026-01-15T10:00:00Z",
    }))
    .unwrap();
    assert_eq!(live.kind, Some(SnapshotKind::Live));

    // absent means disk: a server predating live snapshots never sends it.
    let old: Snapshot = serde_json::from_value(json!({
        "id": "snap-2", "created_at": "2026-01-15T10:00:00Z",
    }))
    .unwrap();
    assert_eq!(old.kind, None);

    // a kind this sdk has never heard of passes through rather than failing
    // the whole decode, so an old client keeps working against a new server.
    let future: Snapshot = serde_json::from_value(json!({
        "id": "snap-3", "kind": "filesystem", "created_at": "2026-01-15T10:00:00Z",
    }))
    .unwrap();
    assert_eq!(future.kind, Some(SnapshotKind::Other("filesystem".into())));
}
