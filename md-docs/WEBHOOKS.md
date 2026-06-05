# XrayTool Webhooks Documentation

This document describes all the webhooks available in the `xraytool-go` platform. The system uses webhooks for two distinct purposes:
1. **Inbound Webhooks:** Receiving events from third-party systems (e.g., payment gateways).
2. **Outbound Webhooks:** Sending internal application events (e.g., device limits reached, successful payments) to external systems (e.g., your Python Telegram bot, analytics platforms).

---

## 1. Outbound Webhooks (Event Dispatcher)

The application features a built-in event dispatcher (`internal/events/Dispatcher`) that broadcasts structured JSON events to a configured list of URLs via HTTP POST.

### ⚙️ Configuration
Outbound webhooks are configured in your `config.yaml` file under the `webhooks` array. You can specify multiple endpoints:

```yaml
webhooks:
  - "http://127.0.0.1:8000/api/bot/webhook"
  - "https://analytics.example.com/events"
```
*If a webhook endpoint is unreachable, the system will retry up to 3 times before dropping the event.*

### 📦 Payload Format
Every outbound webhook is sent as an HTTP `POST` request with a `Content-Type: application/json` header. The body is a JSON object matching the `Event` structure:

```json
{
  "event_id": "evt_uuid-v4-string",
  "event_type": "name.of.the.event",
  "timestamp": "2026-06-05T12:00:00Z",
  "data": { ... },
  "user_metadata": { ... } // Optional: Telegram ID, etc.
}
```

### 🔔 Available Outbound Events

#### `payment.completed`
* **Trigger:** Dispatched asynchronously when a user's payment successfully completes and their balance is updated.
* **`data` payload:**
  * `payment_id` (string): UUID of the payment.
  * `amount` (int): Payment amount in minimal units.
  * `payment_type` (string): Type of payment (`balance`, `subscription`).
  * `method` (string): Payment gateway method used (e.g., `platega`).
  * `user_id` (string): UUID of the user.

#### `device.limit_reached`
* **Trigger:** Dispatched *synchronously* when a user tries to connect via a VPN client (V2Ray, Shadowrocket, etc.) but has exceeded their active device limit (`MaxDevices`).
* **`data` payload:**
  * `email` (string): The generated Xray client email.
  * `client_id` (string): The `xray_uuid`.
  * `subfile` (string): The subscription filename requested.
  * `hwid` (string): Hardware ID of the rejected device.
  * `device_limit` (int): The maximum allowed devices for this user.
  * `device_model`, `device_os`, `ver_os`, `user_agent` (strings): Telemetry data parsed from the client request.
* **`user_metadata` payload:** Contains the user's `Metadata` column from the database (usually includes `telegram_id`, `telegram_username`, etc., making it easy for bots to send alert messages directly to the user).

#### `platega.callback`
* **Trigger:** Dispatched asynchronously whenever the inbound Platega webhook receives a valid callback.
* **`data` payload:** Contains the raw, unparsed JSON body directly forwarded from the Platega payment gateway.

---

## 2. Inbound Webhooks

Inbound webhooks are REST API endpoints that third-party systems call to notify `xraytool-go` about external state changes.

### 💳 Platega Payment Gateway Callback
* **Endpoint:** `POST /api/v1/payments/platega/callback`
* **Authentication:** This route does **NOT** require the standard `X-API-Key` header. Instead, it relies on an `X-Platega-Signature` header to authenticate that the payload actually originated from the Platega gateway.
* **Behavior:**
  1. Validates the signature header.
  2. Parses the incoming JSON.
  3. Finds the payment by `external_id`.
  4. Atomics updates the payment status to `completed`.
  5. Updates the user's balance and executes any auto-renew subscriptions if applicable.
  6. Dispatches the `payment.completed` and `platega.callback` outbound events.
