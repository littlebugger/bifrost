pool "internal" {
  balance                 = "roundrobin"
  backend_tls             = "starttls-verify"
  backend_tls_server_name = "mta1.example.com"
  backend_tls_ca          = "does-not-exist-ca.crt"
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

routing {
  default_pool = "internal"
}
