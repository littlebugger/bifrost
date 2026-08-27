pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
    backup  = true
  }
}

routing {
  default_pool = "internal"
}
