defaults {
  timeouts {
    client_idle        = "300s"
    session_max        = "1h"
    backend_connect    = "5s"
    backend_handshake  = "15s"
    backend_mail_reply = "30s"
    backend_354_wait   = "60s"
    data_progress      = "60s"
    backend_final_dot  = "600s"
    lame_duck          = "2s"
    drain_timeout      = "30s"
  }
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
