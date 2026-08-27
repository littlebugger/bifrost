pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

pool "internal" {
  balance = "leastconn"
  server "mta2" {
    address = "192.0.2.2:25"
  }
}

routing {
  default_pool = "internal"
}
