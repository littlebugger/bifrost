pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
    check {
      interval = "5s"
      timeout  = "10s"
    }
  }
}

routing {
  default_pool = "internal"
}
