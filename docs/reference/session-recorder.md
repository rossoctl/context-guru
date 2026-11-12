# Session recorder (`--record`, `--record-otlp`)

The recorder is **only in a build made with `-tags cg_record`**. The default build, the
release binary and `make build` do not have it.

## Why

The recorder links OpenTelemetry, gRPC and protobuf. They add 11.5 MB (about 25%) to the
binary. Only the internal deployment needs the recorder, so the default build leaves it out.

## Build it

```bash
make build-record
# or
CGO_ENABLED=0 go build -tags cg_record -o bin/context-guru-proxy ./cmd/context-guru-proxy
```

## What a default build does with the flags

- `--record`, `--record-otlp` and the other `--record-*` flags still parse.
- If any of them is set, the proxy logs **one** warning:
  `this build has no session recorder; rebuild with -tags cg_record`.
- The proxy then starts normally, with recording off. It never fails startup.

## What stays in every build

- The `record_traces` account setting, its database column and its settings checkbox.
  An account's opt-in survives a switch between builds.
- The request-path code that checks for a recorder. It sees none and does nothing.

## Leaving one session out

An account that opted in is recorded in every session. There are two ways to leave one
session out.

**`POST /api/me/record/off` with `{"session": "<id>"}`.** The id is the one Claude Code gives
the session (`CLAUDE_CODE_SESSION_ID`), which is also the name of the session's file.

- The proxy writes `<session>.no-record` beside the session's file, then deletes
  `<session>.wire.jsonl`. It writes no later call of that session.
- The marker keeps the session off after a proxy restart. An operator can see it.
- The caller is authenticated like the Ping button, for its own account only. A `?tenant=`
  override other than `me` is refused. An opt-out from a recorded account is never refused for
  rate, so a session-start hook can call it in every session, also on resume. Calls from an
  account that is not recorded are rate-limited per account.
- The answer is `200 {"recording": <bool>, "deleted": <bool>}`. `recording` tells whether the
  account is recorded: false when the proxy has no recorder, and false for an account that did
  not opt in.
- For an account that is not recorded, the proxy writes no marker. It still deletes a file that
  exists, for example one recorded before the account opted out of recording.
- Spans already sent to a collector (`--record-otlp`) cannot be called back. With no
  `--record` directory, the opt-out holds only until the proxy restarts.

**`x-context-guru-record: off` on a request** (`false`, `no` and `0` work too, in any case).
The proxy does not record that request. Earlier requests of the session that did not carry the
header stay recorded; to delete them, use the `POST` above. Any other value, including a typo,
is ignored, so a mistake never stops a recording that was wanted. Like every `x-context-guru-*` header, it is
not forwarded upstream.

Both can only narrow consent. Neither turns recording on for an account that did not opt in,
and the operator's `--record` stays the outer switch.

## Tests

- `go test -tags cg_record ./proxy/... ./internal/record/...` tests the recorder.
- `TestDefaultBuildLinksNoOpenTelemetryGRPCOrProtobuf` fails if the default build links
  any OpenTelemetry, gRPC or protobuf package.

See [data-dependence.md](../data-dependence.md) for what the recorder stores.
