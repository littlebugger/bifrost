# allow_cleartext = true on both auth legs: listener auth WITHOUT a
# starttls block, and pool auth with backend_tls = "none". Both would
# normally be load errors (see auth.hcl / bad-auth-no-starttls.hcl /
# bad-pool-auth-cleartext.hcl); the knob lifts both guards for links
# secured at the network layer (in-cluster k8s).
listener {
  bind     = "127.0.0.1:0"
  hostname = "bifrost.test"

  auth {
    allow_cleartext = true
    user "rttskr-team" {
      salt            = "aa11"
      hashed_password = "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
    }
  }
}

pool "outgoing" {
  balance     = "roundrobin"
  backend_tls = "none"
  auth {
    allow_cleartext = true
    username        = "rttskr-team"
    password        = "pa55w0rd"
  }
  server "s1" { address = "192.0.2.1:25" }
}

routing { default_pool = "outgoing" }
