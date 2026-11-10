# Backend connection reuse -- session-affine backend connections instead
# of one dial per envelope. See docs/operations.md § Backend connection
# reuse.
#
# reuse_envelopes = N (N > 1) keeps a backend connection open across up
# to N envelopes from the same client session, instead of dialing and
# QUIT-ing fresh for each one. Every reuse attempt is revalidated with
# RSET first, so a dead cached connection fails over transparently to a
# normal dial. Omitted, 0, or 1 (the default) means a fresh connection
# per envelope.

listener {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com"
}

pool "outgoing" {
  balance = "roundrobin"

  reuse_envelopes = 50

  server "mta1" {
    address = "192.0.2.11:25"
  }
}

routing {
  default_pool = "outgoing"
}

admin {
  bind = "127.0.0.1:8081"
}
