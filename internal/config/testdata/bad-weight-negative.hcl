pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
    weight  = -1
  }
}

routing {
  default_pool = "internal"
}
