pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
    check {
      rise = 0
      fall = 3
    }
  }
}

routing {
  default_pool = "internal"
}
