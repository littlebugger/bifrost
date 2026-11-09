# resolve_interval below the 1s floor.
pool "internal" {
  balance = "roundrobin"
  server_dns "node" {
    address          = "mta.headless.svc.cluster.local:587"
    resolve_interval = "500ms"
  }
}

routing {
  default_pool = "internal"
}
