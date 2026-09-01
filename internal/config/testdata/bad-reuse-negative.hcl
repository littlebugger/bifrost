pool "internal" {
  balance         = "roundrobin"
  reuse_envelopes = -1
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

routing {
  default_pool = "internal"
}
