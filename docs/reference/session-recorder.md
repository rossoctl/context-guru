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

An account that opted in is recorded in every session. To leave one session out, send
`x-context-guru-record: off` on its requests (`false`, `no` and `0` work too, in any case).
The proxy then writes nothing for that request.

- It can only narrow consent. It never turns recording on for an account that did not opt
  in, and the operator's `--record` stays the outer switch.
- Any other value, including a typo, is ignored, so a mistake never stops a recording
  that was wanted.
- The header is not forwarded upstream, like every `x-context-guru-*` header.

## Tests

- `go test -tags cg_record ./proxy/... ./internal/record/...` tests the recorder.
- `TestDefaultBuildLinksNoOpenTelemetryGRPCOrProtobuf` fails if the default build links
  any OpenTelemetry, gRPC or protobuf package.

See [data-dependence.md](../data-dependence.md) for what the recorder stores.
