# An auth user with an empty salt.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"

  starttls {
    cert = "../../../examples/server.crt"
    key  = "../../../examples/server.key"
  }
  auth {
    user "rttskr-team" {
      salt            = ""
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
