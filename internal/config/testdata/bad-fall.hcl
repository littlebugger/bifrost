pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
    check {
      rise = 2
      fall = 0
    }
  }
}

routing {
  default_pool = "internal"
}
