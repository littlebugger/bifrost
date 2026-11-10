# server_dns address must be a hostname: a bracketed IPv6 literal is
# still an IP literal (net.SplitHostPort strips the brackets before
# netip.ParseAddr sees it; see validateServerDNSAddress).
pool "internal" {
  balance = "roundrobin"
  server_dns "node" {
    address = "[::1]:587"
  }
}

routing {
  default_pool = "internal"
}
