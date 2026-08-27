pool "internal" {
  balance     = "roundrobin"
  backend_tls = "starttls-verify"
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

routing {
  default_pool = "internal"
}
