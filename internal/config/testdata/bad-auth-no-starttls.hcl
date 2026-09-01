# listener.auth without a starttls block: client credentials would cross
# the wire before TLS.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"

  auth {
    user "rttskr-team" {
      salt            = "aa11"
      hashed_password = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
    }
  }
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "starttls"
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
