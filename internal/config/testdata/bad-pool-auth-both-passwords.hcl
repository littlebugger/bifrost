# Pool auth sets both password and password_file: mutually exclusive.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "starttls"
  auth {
    username      = "rttskr-team"
    password      = "pa55w0rd"
    password_file = "pool-password.txt"
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
