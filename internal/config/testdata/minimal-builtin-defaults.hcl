# Every optional attribute is omitted; Load must resolve every one of
# them to the PROJECT.md built-in default (see TestBuiltinDefaults).
listener {
  bind     = "0.0.0.0:25"
  hostname = "mail.example.com"
}

pool "internal" {
  balance = "roundrobin"
  server "mta1" {
    address = "192.0.2.1:25"
  }
}

routing {
  default_pool = "internal"
}
