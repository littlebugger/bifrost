# Pool auth password_file points at a path that doesn't exist.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "starttls"
  auth {
    username      = "rttskr-team"
    password_file = "does-not-exist-password.txt"
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
