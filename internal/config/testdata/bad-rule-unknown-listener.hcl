listener "default" {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com"
}

pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

routing {
  rule {
    listener = ["does-not-exist"]
    pool     = "internal"
  }
  default_pool = "internal"
}
