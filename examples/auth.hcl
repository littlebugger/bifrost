# SMTP AUTH -- client leg and backend leg, configured independently.
# See docs/operations.md § SMTP AUTH.
#
# Client leg (listener.auth): bifrost terminates AUTH PLAIN itself, over
# TLS only. Each user is a salt plus a SHA-256 hash of (salt + password),
# never the plaintext -- mint one with:
#   python3 -c "import secrets,hashlib; s=secrets.token_hex(16); \
#     print(s, hashlib.sha256((s+'correct-horse-battery-staple').encode()).hexdigest())"
# The pair below was minted that way; the plaintext password is
# "correct-horse-battery-staple". Configuring auth makes it required:
# every MAIL before a successful AUTH gets 530 until the client
# authenticates.
#
# Backend leg (pool.auth): bifrost authenticates *to the backend* with
# its own, separate, plaintext credential -- the two legs are never
# relayed into each other. backend_tls must not be "none": the password
# crosses the wire in the clear, only over that encrypted leg.

listener {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com"

  starttls {
    cert = "server.crt"
    key  = "server.key"
  }

  auth {
    user "rttskr-team" {
      salt            = "2276f51497ef56fe9b497b80f1ef9ab4"
      hashed_password = "ce32ec4d356f4e79d2d6ed5981ff04ea8c93072a72d2d59c5dfba0c71de89b27"
    }
  }
}

pool "backend" {
  balance = "roundrobin"

  backend_tls = "starttls"

  auth {
    username = "rttskr-team"
    password = "pa55w0rd"
  }

  server "mta1" {
    address = "192.0.2.11:25"
  }
}

routing {
  default_pool = "backend"
}

admin {
  bind = "127.0.0.1:8081"
}
