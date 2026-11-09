# server_dns address must be a hostname: an IP literal belongs on a
# plain server block instead (see validateServerDNSAddress).
pool "internal" {
  balance = "roundrobin"
  server_dns "node" {
    address = "192.0.2.1:587"
  }
}

routing {
  default_pool = "internal"
}
