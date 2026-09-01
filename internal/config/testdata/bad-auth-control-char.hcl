# Pool auth username carrying an injected control character (backend AUTH
# exchange would relay this verbatim).
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "starttls"
  auth {
    username = "rttskr-team\r\nMAIL FROM:<injected>"
    password = "pa55w0rd"
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
