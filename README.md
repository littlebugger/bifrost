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
