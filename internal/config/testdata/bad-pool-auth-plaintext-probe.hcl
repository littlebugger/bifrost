# Pool auth with backend_tls = "starttls" (fine for traffic) but a check
# override of tls = "none": probes would carry the pool credentials to a
# cleartext EHLO.
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "starttls"
  auth {
    username = "rttskr-team"
    password = "pa55w0rd"
  }
  check {
    tls = "none"
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
