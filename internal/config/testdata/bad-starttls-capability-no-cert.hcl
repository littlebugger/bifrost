# STARTTLS advertised with no certificate to back it: clients would get a
# 502 for a capability the listener promised.
listener {
  bind         = "0.0.0.0:25"
  hostname     = "mail.example.com"
  capabilities = ["PIPELINING", "STARTTLS"]
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
