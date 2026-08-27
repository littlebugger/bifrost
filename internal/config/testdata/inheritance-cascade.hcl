# Exercises the full server > pool > defaults > built-in cascade for
# check params, plus Check.TLS/EhloName falling back to the pool's own
# BackendTLS/EhloName rather than to defaults.check (see
# TestDefaultsToPoolToServer).
defaults {
  ehlo_name = "defaults.example.com"

  check {
    interval      = "5s"
    down_interval = "15s"
    timeout       = "5s"
    rise          = 2
    fall          = 3
    level         = "ehlo"
  }
}

listener {
  bind     = "0.0.0.0:25"
  hostname = "listener.example.com"
}

pool "p1" {
  balance     = "roundrobin"
  backend_tls = "starttls"

  check {
    interval = "10s"
    timeout  = "10s"
  }

  server "s-pool-override" {
    address = "192.0.2.1:25"
  }

  server "s-server-override" {
    address = "192.0.2.2:25"
    check {
      interval = "20s"
      timeout  = "20s"
    }
  }
}

pool "p2" {
  balance = "roundrobin"

  server "s-defaults-only" {
    address = "192.0.2.3:25"
  }
}

routing {
  default_pool = "p1"
}
