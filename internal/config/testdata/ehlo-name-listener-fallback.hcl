# No defaults.ehlo_name and no pool-level ehlo_name: Pool.EhloName must
# fall all the way back to the listener's hostname.
listener {
  bind     = "0.0.0.0:25"
  hostname = "listener-fallback.example.com"
}

pool "p1" {
  balance = "roundrobin"
  server "s1" {
    address = "192.0.2.1:25"
  }
}

routing {
  default_pool = "p1"
}
