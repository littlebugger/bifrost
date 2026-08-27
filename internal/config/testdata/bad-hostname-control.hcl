# The hostname is written verbatim into the banner: a CRLF in it would
# inject a reply line.
listener {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com\r\n250 injected"
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
