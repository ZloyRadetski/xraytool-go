# xraytool external-plugin Go SDK

Import this package from an external plugin binary:

```go
import pluginrpc "github.com/ZloyRadetski/xraytool-go/plugins-external/sdk"
```

Implement `pluginrpc.Implementation` and call `pluginrpc.Serve` from `main`.
The package is a standalone Go module under `plugins-external`; it deliberately
does not depend on the xraytool application module or on an `internal` package.

`CallRequest` and `CallResponse` carry only JSON-compatible payloads. A plugin
may use `InitRequest.Services` only for services declared in `Metadata.Requires`
and explicitly exposed by the host. This is a structured RPC bridge, not a
mechanism for transporting arbitrary Go interfaces or database handles.

The only v1 exception is an external plugin that publishes
`antifraud_provider`: the host automatically supplies the `ban_update_sink`
service during `Init`, without putting it in `Requires`. It supports
`push_ban_update` with `{email, banned_until}` (`banned_until` is RFC3339 or a
whole Unix-second value) and `push_unban` with `{email}`. No other undeclared
host service is available.

The authoritative wire contract is
[`proto/xraytool/plugin/v1/plugin.proto`](../../proto/xraytool/plugin/v1/plugin.proto).
Update the SDK and host together whenever `ProtocolVersion` changes.
