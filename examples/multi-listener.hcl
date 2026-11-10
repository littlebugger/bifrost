# Multiple labeled listeners -- separate binds for separate traffic
# shapes, routed to separate pools by a routing rule's `listener`
# attribute. See README.md's Configuration table (this feature has no
# docs/operations.md section yet).
#
# Each listener block needs a label the moment there is more than one;
# bind and name must each be unique across the set. routing.rule.pool
# only applies to sessions accepted on one of `listener`'s named
# binds; everything else falls through to default_pool.

listener "transactional" {
  bind     = "0.0.0.0:2525"
  hostname = "transactional.mail.example.com"
}

listener "bulk" {
  bind     = "0.0.0.0:2526"
  hostname = "bulk.mail.example.com"
}

pool "transactional" {
  balance = "roundrobin"

  server "mta1" {
    address = "192.0.2.11:25"
  }
}

pool "bulk" {
  balance = "leastconn"

  server "mta2" {
    address = "198.51.100.21:25"
  }
}

routing {
  rule {
    listener = ["bulk"]
    pool     = "bulk"
  }

  default_pool = "transactional"
}

admin {
  bind = "127.0.0.1:8081"
}
