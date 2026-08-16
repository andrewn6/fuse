# fuse rust sdk

rust client for the fuse microvm control plane. mirrors the go sdk.

the crate is published as `folsom-fuse`; the library target is `fuse`, so
`use fuse::Client` mirrors the python sdk's `import fuse`.

## install

```sh
cargo add folsom-fuse
```

## usage

```rust
use fuse::{Client, CreateRequest, Spec};

#[tokio::main]
async fn main() -> Result<(), fuse::Error> {
    let client = Client::builder("https://orchestrator.example.com")
        .token("...")
        .build()?;

    let env = client
        .environments()
        .create(CreateRequest::new("t-1").spec(Spec::new().cpus(2).ram_mb(2048)))
        .await?;

    let mut events = client.environments().events(&env.id).await?;
    while let Some(event) = events.next().await {
        let event = event?;
        println!("{:?}", event.state);
        if event.state.as_ref().is_some_and(|s| s.is_terminal()) {
            break;
        }
    }
    Ok(())
}
```

services hang off the client: `client.environments()`, `client.snapshots()`,
`client.hosts()`, `client.api_keys()`.

every call returns `Result<T, fuse::Error>`; a non-2xx response is
`Error::Api` carrying the server's error envelope, with predicates like
`err.is_not_found()` to branch on the code.

## development

```sh
cargo test
cargo clippy --all-targets -- -D warnings
cargo fmt --check
```
