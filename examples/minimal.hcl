# Minimal load-valid config: one listener, one pool, one routing default.
# This is the exact snippet embedded in README.md's "Configuration"
# section, kept here so TestExamplesLoad (internal/config/examples_test.go)
# proves it never silently rots out of sync with required-attribute
# changes (hostname and balance are both required, not optional).

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
