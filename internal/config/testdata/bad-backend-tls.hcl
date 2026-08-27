pool "internal" {
  balance     = "roundrobin"
  backend_tls = "bogus"
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

routing {
  default_pool = "internal"
}
