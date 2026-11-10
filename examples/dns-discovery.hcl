# DNS-discovered pool members -- one server_dns template expands at
# runtime into one pool member per resolved IP, instead of one
# hand-written server block per destination. See docs/operations.md §
# DNS-discovered pool members.
#
# address's host must be a hostname (an IP literal is a load error --
# use a plain server {} block for a fixed IP instead). It is never
# resolved at config load; resolution happens at runtime on a fixed,
# jittered poll of resolve_interval (default 30s, 1s floor). The
# typical source is a Kubernetes headless Service, whose name answers
# with one A/AAAA record per ready Pod.

listener {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com"
}

pool "routes" {
  balance = "leastconn"

  server_dns "node" {
    address          = "mta.headless.svc.cluster.local:587"
    resolve_interval = "30s"
    weight           = 1
  }
}

routing {
  default_pool = "routes"
}

admin {
  bind = "127.0.0.1:8081"
}
