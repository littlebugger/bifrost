# Bifrost

*An SMTP transaction-splicing load balancer.*

![CI](https://github.com/littlebugger/bifrost/actions/workflows/ci.yml/badge.svg)

An SMTP-aware, cut-through load balancer. It sits between SMTP clients and a pool of destination SMTP servers, balances **each mail transaction independently** — even when a client sends thousands of messages over one long-lived connection — and relays server verdicts to the client **verbatim and immediately**, in both directions. Think *HAProxy's operability and configurability, applied at the SMTP-transaction layer instead of the TCP-connection layer*.

Read `PROJECT.md` for the full spec; implementation plans live in `docs/plans/`.

## Install

```sh
go install github.com/littlebugger/bifrost/cmd/bifrost@latest
```

Or download a prebuilt binary from [GitHub Releases](https://github.com/littlebugger/bifrost/releases).

Or build from source:

```sh
make build   # -> ./bin/bifrost
```

## Configuration

Bifrost reads one HCL file. The minimal shape — one listener, one pool,
one routing default (this is `examples/minimal.hcl`, validated by the
test suite):

```hcl
listener {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com"
}

pool "backend" {
  balance = "roundrobin"
  server "mta1" { address = "192.0.2.11:25" }
}

routing {
  default_pool = "backend"
}
```

`examples/bifrost.hcl` is the full annotated reference; each feature
below also has its own minimal, load-valid example:

| File | Feature | docs/operations.md |
| --- | --- | --- |
| `examples/auth.hcl` | SMTP AUTH (client and backend leg) | [§ SMTP AUTH](docs/operations.md#smtp-auth) |
| `examples/reuse.hcl` | Backend connection reuse | [§ Backend connection reuse](docs/operations.md#backend-connection-reuse) |
| `examples/dns-discovery.hcl` | DNS-discovered pool members | [§ DNS-discovered pool members](docs/operations.md#dns-discovered-pool-members) |
| `examples/multi-listener.hcl` | Multiple labeled listeners + routing by listener | [§ Multiple listeners](docs/operations.md#multiple-listeners) |
