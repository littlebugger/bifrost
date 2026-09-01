# Bifrost reference configuration.
#
# This is the annotated example referenced by PROJECT.md and doubles as
# internal/config's primary test fixture (see load_test.go). It exercises
# every top-level block; see PROJECT.md for the full semantics.

defaults {
  # Traffic EHLO identity used when neither a pool nor its servers set
  # their own ehlo_name. Falls back further to listener.hostname if this
  # is also omitted.
  ehlo_name = "mail.example.com"

  # Every row of PROJECT.md's timeout budget table. These are the
  # documented v1 defaults themselves (values below the RFC 5321
  # §4.5.3.2 floor are a deliberate, documented deviation for a balancer
  # between cooperating parties -- see PROJECT.md); config validation
  # only warns about that, it never fails loading.
  timeouts {
    client_idle        = "300s"
    session_max         = "1h"
    backend_connect      = "5s"
    backend_handshake    = "15s"
    backend_mail_reply   = "30s"
    backend_354_wait     = "60s"
    data_progress        = "60s"
    backend_final_dot    = "600s"
    lame_duck            = "2s"
    drain_timeout        = "30s"
  }

  # Health-check defaults inherited by every pool/server that doesn't
  # override them.
  check {
    level         = "ehlo"
    interval      = "5s"
    down_interval = "15s"
    timeout       = "5s"
    rise          = 2
    fall          = 3
  }
}

# Exactly one listener is supported in v1.
listener {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com"

  starttls {
    # cert/key here are example paths only: the throwaway dev certs that
    # normally sit alongside this file in examples/ are not shipped in
    # release archives. Point these at your own certificate and key.
    cert        = "server.crt"
    key         = "server.key"
    min_version = "1.2"
  }

  capabilities = ["PIPELINING", "8BITMIME", "SIZE 10485760", "STARTTLS"]

  # Client-leg SMTP AUTH (PLAIN only), disabled here. Requires the
  # starttls block above -- AUTH PLAIN is advertised only post-STARTTLS,
  # and configuring auth makes it required (MAIL gets 530 until a
  # client authenticates). Mint hashed_password with:
  #   hashed_password = hex(sha256(salt || password))
  # (dev env: `make kumo-credential` mints a salt/hashed_password pair
  # in this same kumo inbound_auth.toml-compatible format). Uncomment to
  # enable:
  # auth {
  #   user "rttskr-team" {
  #     salt            = "1af90c3e2b7ad4f1"
  #     hashed_password = "d989c9f1e4a0b3c7f5e2d1a6b8c4f0e3d7a9c2b5f8e1d4a7c0b3f6e9d2a5c8b1"
  #   }
  # }
}

# "internal": smooth-weighted round robin, plain-TCP probe on an
# alternate port for the spare backup -- the probe-port-override
# showcase.
pool "internal" {
  balance     = "roundrobin"
  backend_tls = "none"
  ehlo_name   = "internal.mail.example.com"

  server "mta1" {
    address = "192.0.2.11:25"
    weight  = 3
  }

  server "mta2" {
    address = "192.0.2.12:25"
    weight  = 1
  }

  server "spare" {
    address = "192.0.2.13:25"
    backup  = true

    check {
      level = "connect"
      port  = 9025
    }
  }
}

# "bulk": least-connections, backend-leg TLS with verification -- the
# backend-TLS showcase.
pool "bulk" {
  balance          = "leastconn"
  max_transactions = 500

  backend_tls             = "starttls-verify"
  backend_tls_server_name = "mail.bulk.example.com"
  backend_tls_ca          = "server.crt"

  server "b1" {
    address = "198.51.100.21:25"
    weight  = 1
  }

  # Backend-leg SMTP AUTH, disabled here. Requires backend_tls != "none"
  # (already set above) -- bifrost sends this password as PLAIN, in the
  # clear, only over that encrypted leg. Unlike the listener's hash,
  # this is the real plaintext credential the backend expects. Uncomment
  # to enable:
  # auth {
  #   username = "rttskr-team"
  #   password = "pa55w0rd"
  # }
}

routing {
  rule {
    client_cidr = ["10.0.0.0/8"]
    pool        = "internal"
  }

  rule {
    mail_from_domain = ["*.news.example.com"]
    pool             = "bulk"
  }

  default_pool = "internal"
}

limits {
  global_maxconn = 2048
}

# Loopback-only unless allow_remote = true.
admin {
  bind = "127.0.0.1:8081"
}
