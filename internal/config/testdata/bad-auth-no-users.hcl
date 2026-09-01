# An auth block with no user sub-blocks: nobody could ever authenticate.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"

  starttls {
    cert = "../../../examples/server.crt"
    key  = "../../../examples/server.key"
  }
  auth {}
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "starttls"
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
