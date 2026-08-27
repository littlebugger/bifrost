pool "internal" {
  balance = "roundrobin"

  server "mta1" {
    address = "192.0.2.11:25"
    wieght  = 3
  }
}
