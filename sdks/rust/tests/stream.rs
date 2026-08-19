//! The desktop stream against a real socket server: wiremock cannot answer a
//! connection upgrade, so these tests speak http/1.1 by hand the way the go
//! and typescript sdk tests do.

use fuse::{Client, Error, ErrorCode};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

const GREETING: &[u8] = b"RFB 003.008\n";

async fn read_head(conn: &mut TcpStream) -> String {
    let mut head = Vec::new();
    let mut byte = [0u8; 1];
    while !head.ends_with(b"\r\n\r\n") {
        conn.read_exact(&mut byte).await.expect("read head");
        head.push(byte[0]);
    }
    String::from_utf8_lossy(&head).into_owned()
}

/// Answers one upgrade with a greeting and then echoes, prefixed.
async fn serve_upgrade(listener: TcpListener) -> String {
    let (mut conn, _) = listener.accept().await.expect("accept");
    let head = read_head(&mut conn).await;
    conn.write_all(
        b"HTTP/1.1 101 Switching Protocols\r\n\
          Upgrade: fuse-vnc/1\r\n\
          Connection: Upgrade\r\n\r\n",
    )
    .await
    .expect("write 101");
    conn.write_all(GREETING).await.expect("write greeting");

    let mut buf = [0u8; 64];
    let n = conn.read(&mut buf).await.expect("read payload");
    conn.write_all(b"echo:").await.expect("write echo prefix");
    conn.write_all(&buf[..n]).await.expect("write echo");
    head
}

#[tokio::test]
async fn computer_stream_relays_both_directions() {
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
    let base_url = format!("http://{}", listener.local_addr().expect("addr"));
    let server = tokio::spawn(serve_upgrade(listener));

    let client = Client::new(base_url, "tok").expect("client");
    let mut stream = client
        .environments()
        .computer_stream("vm-1")
        .await
        .expect("computer_stream");

    // guest -> client: the greeting rides right behind the response head
    let mut greeting = vec![0u8; GREETING.len()];
    stream
        .read_exact(&mut greeting)
        .await
        .expect("read greeting");
    assert_eq!(greeting, GREETING);

    // client -> guest and back
    stream.write_all(b"hello").await.expect("write");
    let mut back = vec![0u8; b"echo:hello".len()];
    stream.read_exact(&mut back).await.expect("read echo");
    assert_eq!(back, b"echo:hello");

    let head = server.await.expect("server");
    assert!(head.starts_with("GET /v1/environments/vm-1/computer/stream HTTP/1.1"));
    assert!(head.to_ascii_lowercase().contains("upgrade: fuse-vnc/1"));
    assert!(head.contains("Bearer tok"));
}

#[tokio::test]
async fn computer_stream_error_surfaces_as_api_error() {
    let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
    let base_url = format!("http://{}", listener.local_addr().expect("addr"));
    tokio::spawn(async move {
        let (mut conn, _) = listener.accept().await.expect("accept");
        let _ = read_head(&mut conn).await;
        let body = br#"{"error":{"code":"unavailable","message":"guest surface unavailable"}}"#;
        conn.write_all(
            format!(
                "HTTP/1.1 503 Service Unavailable\r\n\
                 Content-Type: application/json\r\n\
                 Content-Length: {}\r\n\r\n",
                body.len()
            )
            .as_bytes(),
        )
        .await
        .expect("write head");
        conn.write_all(body).await.expect("write body");
    });

    let client = Client::new(base_url, "tok").expect("client");
    let err = client
        .environments()
        .computer_stream("vm-1")
        .await
        .expect_err("want an error for a 503");
    match err {
        Error::Api(api) => {
            assert_eq!(api.status, 503);
            assert_eq!(api.code, Some(ErrorCode::Unavailable));
            assert!(api.message.contains("guest surface unavailable"));
        }
        other => panic!("err = {other:?}, want Error::Api"),
    }
}

#[tokio::test]
async fn computer_stream_validates_before_request() {
    let client = Client::new("http://fuse.test", "tok").expect("client");
    let err = client
        .environments()
        .computer_stream("")
        .await
        .expect_err("want an error for an empty vm id");
    assert!(matches!(err, Error::InvalidRequest(_)));
}
