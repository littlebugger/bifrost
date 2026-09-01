# Same as auth.hcl, but the pool auth password is sourced from a file
# (Kubernetes secret mount) instead of being written inline.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"

  starttls {
    cert = "../../../examples/server.crt"
    key  = "../../../examples/server.key"
  }
  auth {
    user "rttskr-team" {
      salt            = "aa11"
      hashed_password = "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
    }
  }
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "starttls"
  auth {
    username      = "rttskr-team"
    password_file = "pool-password.txt"
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
