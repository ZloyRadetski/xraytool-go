# External plugin SDK

External plugins are separate processes connected through HashiCorp `go-plugin`
and gRPC. Third-party Go plugins use the standalone SDK package:

```go
import pluginrpc "github.com/ZloyRadetski/xraytool-go/plugins-external/sdk"
```

It does not import the xraytool application module or any `internal` package.
The wire service is documented in
[`proto/xraytool/plugin/v1/plugin.proto`](../proto/xraytool/plugin/v1/plugin.proto).

Build the runnable payment example from the standalone external-plugin module:

```powershell
go -C plugins-external build -o ../xraytool-plugin-payment-stub.exe ./examples/payment_stub
```

For a plugin in a separate repository, add the SDK with `go get
github.com/ZloyRadetski/xraytool-go/plugins-external/sdk@latest` and implement
`pluginrpc.Implementation`. The SDK's `ProtocolVersion`, magic cookie, service
names, and JSON/protobuf envelope format are kept compatible with the host's
`xraytool/pluginrpc` package.

Then configure it as an external plugin (use the actual binary path):

```yaml
plugins:
  payment_stub:
    enabled: true
    source: external
    exec: C:\opt\xraytool\xraytool-plugin-payment-stub.exe
    restart_policy:
      max_restarts: 3
      backoff: 2s
```

The example supports the explicit `payment_provider` adapter. Its `method_id`
comes from `Metadata.Capabilities.method_id`. Supported v1 structured adapters
are `payment_provider`, `pricing_engine`, `notification_provider`, and
`event_sink`. A local service is available to an external plugin only if it is
explicitly adapted as `pluginrpc.ServiceHandler`; ordinary Go interfaces and
repositories are intentionally rejected at startup.

For more than one gateway, publish a unique registry name such as
`payment_provider.yookassa`; the RPC `Service` value remains
`payment_provider`, while `method_id` chooses the host dispatch key.

## V1 call contracts

Every operation uses `pluginrpc.CallRequest{Service, Method, Payload}` and a
`CallResponse{Payload}`. Envelope keys are snake_case.

| Service | Method | Request payload | Response payload |
|---|---|---|---|
| `payment_provider` | `create_intent` | `user_id`, `amount`, `currency`, `description`, `external_ref`, `custom_data` | `external_id`, `payment_url`, optional `raw_response` |
| `payment_provider` | `verify_callback` | `method`, `path`, `raw_query`, `host`, `remote_addr`, `headers`, `body_base64` | `external_id`, `status`, `amount`, `currency`, optional `custom_data` |
| `payment_provider` | `refund` | `external_id`, `amount` | empty payload |
| `pricing_engine` | `calculate_price` | complete immutable pricing snapshot (`user_id`, `plan`, `promo`, `current_subscription`, etc.) | `final_price`, `discount_percent`, `applied_promo`, optional `applied_promo_id`, `description` |
| `notification_provider` | `send` | `channel`, `to`, `kind`, `payload` | empty payload |
| `event_sink` | `handle` | `type`, `occurred_at`, `data`, `user_meta` | empty payload |

The callback body is limited to 1 MiB by the host and encoded as base64. The
original Go request body is restored after it has been serialized. The host
uses `AutoMTLS` for the go-plugin connection and applies a 3-second default
RPC deadline; plugin operations must honour the received gRPC context.
