pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
    check {
      port = 70000
    }
  }
}

routing {
  default_pool = "internal"
}
