pool "internal" {
  balance = "roundrobin"

  server "mta1" {
    weight = 1
  }
}
