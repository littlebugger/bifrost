pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

routing {
  rule {
    client_cidr = ["10.0.0.0/8"]
    pool        = "does-not-exist"
  }
  default_pool = "internal"
}
