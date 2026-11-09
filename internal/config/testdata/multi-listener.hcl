# Two labeled listeners: "transactional" (plain, default capability set)
# and "bulk" (starttls + client auth, custom capabilities). A routing rule
# pins "bulk" traffic at the "stream-b" pool; everything else falls
# through to default_pool (Task 2 makes the routing.listener attribute's
# matching behavior real — this fixture only proves the decode).
listener "transactional" {
  bind     = "127.0.0.1:2500"
  hostname = "transactional.bifrost.test"
}

listener "bulk" {
  bind         = "127.0.0.1:2501"
  hostname     = "bulk.bifrost.test"
  capabilities = ["PIPELINING", "8BITMIME", "SIZE 10485760"]

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

pool "stream-a" {
  balance = "roundrobin"
  server "mta1" { address = "192.0.2.1:25" }
}

pool "stream-b" {
  balance = "roundrobin"
  server "mta2" { address = "192.0.2.2:25" }
}

routing {
  rule {
    listener = ["bulk"]
    pool     = "stream-b"
  }
  default_pool = "stream-a"
}
