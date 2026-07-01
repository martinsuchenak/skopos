# Nomad

The repo includes a Nomad job spec (`skopos.nomad`) for deploying skopos as a service.

## Job spec

```hcl
job "skopos" {
  datacenters = ["dc1"]
  type        = "service"

  group "skopos" {
    count = 1

    network {
      port "http" { to = 8080 }
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
```

## Deploy

```bash
nomad job run skopos.nomad
```

## Notes

- Only port 8080 is exposed (HTTP, MCP at `/mcp`, SSE at `/api/events/stream`).
- The health check hits `/health` every 10s.
- For persistent data, add a volume mount for `/app/skopos.db`.
- Set env vars via the `env` block in the task config:
  ```hcl
  env {
    SKOPOS_API_KEY = "mysecret"
    LOG_LEVEL      = "info"
  }
  ```
