# capabilities omitted AND a certificate configured: the built-in safe
# set plus the one capability Bifrost adds on its own (see
# TestCapabilitiesDefaultWithCertAppendsStartTLS).
listener {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com"

  starttls {
    cert = "../../../examples/server.crt"
    key  = "../../../examples/server.key"
  }
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
