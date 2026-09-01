# SMTP AUTH on both legs: listener auth (client-facing, salted-SHA256
# users) and pool auth (backend-facing, plaintext credentials). The
# listener carries a starttls block because Task 2 makes auth without
# starttls an error.
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
    username = "rttskr-team"
    password = "pa55w0rd"
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
