# Bifrost reference configuration.
#
# This is the annotated example referenced by PROJECT.md and doubles as
# internal/config's primary test fixture (see load_test.go). It exercises
# every top-level block; see PROJECT.md for the full semantics.

defaults {
  # Traffic EHLO identity used when neither a pool nor its servers set
  # their own ehlo_name. Falls back further to the FIRST configured
  # listener's hostname if this is also omitted -- with more than one
  # listener, set ehlo_name explicitly here rather than relying on
  # whichever listener happens to be first.
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

# This file stays single-listener on purpose -- it's internal/config's
# load fixture (see load_test.go), and a lone unlabeled block like this
# one is still exactly how a single-listener deployment is written: it
# gets the implicit name "default", no label required. The moment a
# second listener{} block is added (sketched commented-out below), every
# block needs a label; see examples/multi-listener.hcl and
# docs/operations.md's "Multiple listeners" section for the full shape.
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
  #     hashed_password = "466aec5e9c8096eb07b86d055773ea4267b548c25831c6d56a5c8ff7f5497977"
  #   }
  #   # allow_cleartext = true  # only for a link already secured at the network layer (e.g. in-cluster k8s) -- lifts the starttls requirement and the 538 gate
  # }
}

# A second listener -- its own bind, hostname, and (optionally) its own
# starttls/auth/capabilities -- routed to its own pool by a routing rule's
# `listener` attribute (AND-combined with client_cidr/mail_from_domain).
# Both blocks need a label the moment there is more than one. Uncomment
# to enable:
# listener "bulk" {
#   bind         = "0.0.0.0:2526"
#   hostname     = "bulk.mail.example.com"
#   capabilities = ["PIPELINING", "8BITMIME"]
# }

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

  # reuse_envelopes = 50   # session-affine reuse: keep a backend conn across up to 50 envelopes instead of one-per-envelope

  server "b1" {
    address = "198.51.100.21:25"
    weight  = 1
  }

  # DNS-discovered members, disabled here: a server_dns block is a server
  # *template* -- one DNS name that expands at runtime into one pool
  # member per resolved IP (named node-<ip>, at this template's own
  # port), each a full pool citizen: own health FSM, leases, metrics
  # labels, failover candidate, reuse-affinity target. Typical source is
  # a k8s headless Service (clusterIP: None), whose name answers with one
  # A/AAAA record per ready Pod instead of one VIP. Go's resolver exposes
  # no TTL, so refresh is a fixed, jittered interval (resolve_interval),
  # not TTL-driven; a lookup error or an empty answer keeps the
  # last-known-good membership (fail-static) rather than ever emptying
  # the pool. Mixing with static server {} blocks above is fine. Uncomment
  # to enable:
  # server_dns "node" {
  #   address          = "mta.headless.svc.cluster.local:587"
  #   resolve_interval = "30s"   # default; 1s floor
  #   weight           = 1
  # }

  # Backend-leg SMTP AUTH, disabled here. Requires backend_tls != "none"
  # (already set above) -- bifrost sends this password as PLAIN, in the
  # clear, only over that encrypted leg. Unlike the listener's hash,
  # this is the real plaintext credential the backend expects. Uncomment
  # to enable:
  # auth {
  #   username = "rttskr-team"
  #   password = "pa55w0rd"
  #   # password_file = "/var/run/secrets/smtp/password"  # alternative to password (e.g. a k8s Secret mount); exactly one of the two
  #   # allow_cleartext = true  # only for a link already secured at the network layer (e.g. in-cluster k8s) -- lifts the backend_tls != "none" requirement
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

  # rule { listener = ["bulk"] pool = "bulk" }   # routes by arrival listener; pairs with the commented listener "bulk" block above

  default_pool = "internal"
}

limits {
  global_maxconn = 2048
}

# Loopback-only unless allow_remote = true.
admin {
  bind = "127.0.0.1:8081"
}
