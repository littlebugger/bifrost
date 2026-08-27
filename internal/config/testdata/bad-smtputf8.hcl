listener {
  bind         = "0.0.0.0:25"
  hostname     = "mail.example.com"
  capabilities = ["SMTPUTF8"]
}

pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

routing {
  default_pool = "internal"
}
