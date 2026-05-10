job "skopos" {
  datacenters = ["dc1"]
  type        = "service"

  group "skopos" {
    count = 1

    network {
      port "http" {
        to = 8080
      }
    }

    task "skopos" {
      driver = "docker"

      config {
        image = "skopos:latest"
        ports = ["http"]
      }

      resources {
        cpu    = 256
        memory = 256
      }

      service {
        name = "skopos"
        port = "http"

        check {
          type     = "http"
          path     = "/health"
          interval = "10s"
          timeout  = "2s"
        }
      }
    }
  }
}
