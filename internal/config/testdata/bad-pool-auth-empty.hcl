# Pool auth with an empty password.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "starttls"
  auth {
    username = "rttskr-team"
    password = ""
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
