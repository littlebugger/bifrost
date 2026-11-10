# server_dns templates and servers share one name namespace per pool.
pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
  }
  server_dns "mta1" {
    address = "mta.headless.svc.cluster.local:587"
  }
}

routing {
  default_pool = "internal"
}
