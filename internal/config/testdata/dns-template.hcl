# server_dns templates: one mixed with a static server (pool "routes",
# also proving pool auth creds reach the template's resolved Check the
# same way they reach a server's), and one pool with ONLY a template and
# zero static servers (pool "dns-only" — must still pass the empty-pool
# check).
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"
}

pool "routes" {
  balance     = "leastconn"
  backend_tls = "starttls"

  auth {
    username = "rttskr-team"
    password = "pa55w0rd"
  }

  server_dns "node" {
    address          = "mta.headless.svc.cluster.local:587"
    resolve_interval = "45s"
    weight           = 3
    backup           = true
    max_transactions = 200

    check {
      level = "banner"
    }
  }

  server "static-extra" {
    address = "10.9.8.7:587"
  }
}

pool "dns-only" {
  balance = "roundrobin"

  server_dns "node" {
    address = "mta2.headless.svc.cluster.local:587"
  }
}

routing {
  default_pool = "routes"
}
