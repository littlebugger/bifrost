# Pool auth with backend_tls = "none": backend credentials would be sent
# in cleartext.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "none"
  auth {
    username = "rttskr-team"
    password = "pa55w0rd"
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
