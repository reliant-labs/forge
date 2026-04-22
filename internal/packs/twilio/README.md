# Twilio Pack

Twilio SMS and voice integration with a typed client, HMAC-SHA1 webhook signature validation, delivery status tracking, and a clean messaging service abstraction.

## Installation

```bash
forge pack install twilio
```

## What Gets Generated

| File | Description |
|------|-------------|
| `pkg/twilio/client.go` | Twilio SDK wrapper for sending SMS and initiating voice calls |
| `pkg/twilio/webhook.go` | Webhook handler with HMAC-SHA1 signature verification for delivery status callbacks |
| `internal/messaging/contract.go` | `Service` interface abstracting SMS/voice operations |
| `internal/messaging/service.go` | `TwilioService` implementing the messaging contract |
| `handlers/webhooks/twilio_webhook_gen.go` | Webhook route registration (regenerated on `forge generate`) |
| `proto/db/v1/twilio_entities.proto` | `SmsMessage` entity for tracking message status and delivery |

## Configuration

```yaml
twilio:
  account_sid_env: TWILIO_ACCOUNT_SID
  auth_token_env: TWILIO_AUTH_TOKEN
  from_number_env: TWILIO_FROM_NUMBER
  status_callback_path: /webhooks/twilio/status
```

| Variable | Purpose |
|----------|---------|
| `TWILIO_ACCOUNT_SID` | Twilio account SID |
| `TWILIO_AUTH_TOKEN` | Twilio auth token (also used for webhook signature verification) |
| `TWILIO_FROM_NUMBER` | Default sender phone number |

## Usage

### Sending SMS

```go
client, err := twilio.NewClient(logger)
sid, err := client.SendSMS("+15551234567", "Hello from Forge!",
    twilio.WithStatusCallback("https://example.com/webhooks/twilio/status"),
)
```

Or use the service abstraction for a provider-independent interface:

```go
svc := messaging.NewTwilioService(client, logger)
sid, err := svc.SendSMS(ctx, "+15551234567", "Hello!")
```

### Voice Calls

```go
sid, err := client.SendVoice("+15551234567", "<Response><Say>Hello!</Say></Response>")
```

### Webhook Status Tracking

Register the webhook route to receive delivery status updates from Twilio:

```go
webhooks.RegisterTwilioWebhooks(mux, messagingService, logger)
```

The handler validates the `X-Twilio-Signature` header using HMAC-SHA1 per Twilio's security spec, then dispatches the callback through the `Service.HandleStatusCallback` method for persistence.

## Dependencies

- `github.com/twilio/twilio-go`

## Removal

```bash
forge pack remove twilio
```
