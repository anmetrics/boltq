# Authentication and authorisation

Before the messaging subsystem, BoltQ had one security control: a shared API
key. Any client holding it could publish to and consume from every queue. That
is workable for a trusted backend fleet and completely unworkable once untrusted
end-user devices connect directly — which is what a chat or dating app requires.

This guide covers the replacement: signed per-user tokens plus an explicit
capability grid.

## The two authentication paths

| Path | Who | Access |
|---|---|---|
| Shared API key | Trusted backend services | Everything, if `allow_anonymous` |
| Signed token | End-user devices | Whatever the ACL grants |

**Never ship the shared API key to a device.** A principal authenticated that
way is `Anonymous` and bypasses the ACL entirely when `allow_anonymous` is on.
It exists to preserve the existing backend model, not to authenticate users.

The gateway refuses to start with `identity.enabled: false`, precisely so a
public WebSocket endpoint cannot accidentally be an unauthenticated message bus.

## Tokens

BoltQ verifies HMAC-SHA256 JWTs. Your auth service mints them; BoltQ only
checks them.

```json
{
  "sub": "user-8891",
  "did": "phone-a1b2c3",
  "tid": "tenant-eu",
  "scp": ["publish", "subscribe", "presence"],
  "iat": 1730000000,
  "exp": 1730003600,
  "iss": "auth.example.com",
  "jti": "tok-4f2a"
}
```

| Claim | Meaning |
|---|---|
| `sub` | User ID. **Required.** This is the identity that owns messages. |
| `did` | Device ID. Becomes the cursor member — must be stable per device. |
| `tid` | Tenant, for multi-tenant deployments. |
| `scp` | Scopes. Omitted means `publish`, `subscribe`, `presence`. |
| `exp` | Expiry. Re-checked on **every frame**, not just at connect. |
| `jti` | Token ID, for revocation. |

Registered claim names are used wherever RFC 7519 defines one, so tokens from
Auth0, Firebase or a homegrown service need no translation layer.

### Scopes

| Scope | Grants |
|---|---|
| `publish` | Appending to streams, publishing to queues |
| `subscribe` | Reading streams, subscribing |
| `admin` | Topic and cluster administration |
| `presence` | Publishing presence and typing signals |

`admin` is **never granted implicitly**. A token with no `scp` claim gets the
three an end-user client needs and nothing more.

### Security properties

The verifier does not trust the token's `alg` header. The algorithm is fixed at
verification time, which closes the `alg: none` bypass and the RS256-as-HS256
confusion attack. Signature comparison is constant-time. Keys shorter than 32
bytes are rejected at construction.

All of this is covered by tests: `TestVerifyRejectsAlgNone`,
`TestVerifyRejectsUnexpectedAlg`, `TestVerifyRejectsTamperedClaims`,
`TestShortKeyRejected`.

### Key rotation

Configure two keys during the overlap:

```json
{
  "messaging": {
    "identity": {
      "enabled": true,
      "issuer": "auth.example.com",
      "keys": [
        { "id": "2024-q4", "secret_env": "BOLTQ_KEY_2024Q4" },
        { "id": "2025-q1", "secret_env": "BOLTQ_KEY_2025Q1" }
      ]
    }
  }
}
```

Mint with the new key; both verify. Remove the old one only after the longest
token lifetime has elapsed — retiring it earlier invalidates outstanding tokens
immediately.

Use `secret_env`, not `secret`, in production. A secret in a config file gets
committed, or shipped inside a container image, or both.

### Minting a token in Go

```go
import "github.com/boltq/boltq/internal/identity"

tok, err := identity.Sign(
    identity.SigningKey{ID: "2025-q1", Secret: []byte(os.Getenv("BOLTQ_KEY_2025Q1"))},
    identity.Claims{
        Subject:   userID,
        DeviceID:  deviceID,
        Scopes:    []string{identity.ScopePublish, identity.ScopeSubscribe, identity.ScopePresence},
        Issuer:    "auth.example.com",
        ExpiresAt: time.Now().Add(time.Hour).Unix(),
    },
)
```

Keep lifetimes short — an hour is reasonable. Long-lived connections re-check
expiry on every frame, so a client must refresh and reconnect. That is the
correct behaviour: without it, a token would effectively never expire for
exactly the long-lived connections that most need it to.

## The ACL

Authorisation is a list of rules evaluated against `(principal, topic, action)`.
Actions are `read`, `write` and `manage`.

### Evaluation

1. Token expired → deny.
2. Anonymous principal → allow if `allow_anonymous`, else deny.
3. Any matching **deny** rule → deny.
4. Any matching **allow** rule → allow.
5. Otherwise → **deny**.

Default deny. A policy with no rules denies everything — a policy that failed
open would be worse than none, because it would look like protection.

Deny is checked before allow so a narrow prohibition cannot be defeated by a
broad permission placed after it.

### Patterns

Dot-separated segments. `*` matches exactly one segment; `#` matches zero or
more trailing segments. Placeholders expand from the principal before matching:
`${user}`, `${device}`, `${tenant}`.

```
chat.inbox.${user}.#     →  each user's own inbox, from one rule
chat.group.*.#           →  any group conversation
#                        →  everything
```

### Exceptions

`ExceptPattern` carves a hole in a rule. This exists because deny-wins makes a
broad deny unable to spare a narrow case:

```go
{ Pattern: "chat.inbox.#", ExceptPattern: "chat.inbox.${user}.#", Effect: Deny }
```

"Nobody may touch an inbox, except your own" cannot be two rules — the deny
would always win over the allow. As one rule with an exception, the intent
survives however many allow rules follow it.

### Membership

Patterns cannot express "is this user in this conversation". That fact lives in
your database. Rules mark it explicitly:

```go
{ Pattern: "chat.group.*.#", Effect: Allow,
  RequireMembership: true, MembershipSegment: 2,
  Actions: []Action{ActionRead, ActionWrite} }
```

`MembershipSegment: 2` means segment index 2 of the topic — the group ID in
`chat.group.<id>`.

**A membership backend that is down denies access.** It never becomes an
authorisation bypass (`TestPolicyMembershipFailureDeniesAccess`). Likewise, a
policy that references membership with no checker configured fails closed
(`TestPolicyMissingMembershipCheckerFailsClosed`).

## The built-in chat policy

`identity.ChatPolicyRules()` is applied by default:

| Topic | Access |
|---|---|
| `chat.inbox.<user>` | Read/write by that user only |
| `chat.inbox.<anyone else>` | Explicitly denied |
| `chat.direct.<conv>` | Read/write, membership-gated |
| `chat.group.<group>` | Read/write, membership-gated |
| `presence.<user>` | That user writes; anyone reads |
| `typing.<conv>` | Read/write, membership-gated |
| `system.*` | Read only for users |
| anything | `manage` requires the `admin` scope |

Presence readability is asymmetric on purpose — being able to observe everyone's
presence *is* a contact list. If your product needs presence restricted to
mutual matches, replace that rule and enforce it through membership.

## The membership service

You must provide two endpoints:

```
GET <base>/is-member?tenant=&user=&group=   →  {"member": true}
GET <base>/members?tenant=&group=           →  {"members": ["u1","u2"]}
```

They are separate because their costs differ by orders of magnitude:
`is-member` is a point lookup on every message, `members` is a scan run only on
fan-out. Collapsing them would make every authorisation check pay for a full
member list.

```json
{ "messaging": { "chat": {
    "membership_url": "https://api.example.com/boltq/membership",
    "membership_timeout": "3s",
    "membership_auth_header": "Bearer <service-token>"
} } }
```

Answers are cached (`identity.membership_cache_ttl`, default 30s). The TTL is
the window in which a removed user can still act on a group — keep it short,
and invalidate explicitly on removal if you need immediate effect.

### Direct conversations need no service

Conversation IDs of the form `alice:bob` resolve their members from the ID
itself. A dating app that is entirely 1:1 needs no membership service at all.
Use sorted user IDs joined by `:` so both participants derive the same ID.

## Common mistakes

**Shipping the shared API key to devices.** Every device becomes an
administrator.

**Long-lived tokens.** Revocation is per-node and in-memory; short lifetimes
are the real control.

**Unstable device IDs.** A device ID that changes on reinstall resets that
user's read positions and creates an orphan cursor per install.

**Trusting `sub` from an unverified source.** Verify the token; never take a
user ID from a request body.

**Leaving `allow_anonymous: true` while exposing the gateway publicly** without
keeping the API key strictly server-side.

## Further reading

- [Building a chat app](building-a-chat-app.md).
- [Gateway protocol](../reference/gateway-protocol.md).
- [Topic conventions](../reference/topics.md).
