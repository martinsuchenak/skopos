# skopos on Nomad, with the SQLite database on the allocation's local disk and
# Litestream streaming every commit to a shared volume (NFS, SMB, …). SQLite
# itself must never sit on the shared volume: every commit becomes a network
# round trip, and WAL mode can corrupt there. See docs/deployment/nomad.md.
job "skopos" {
  datacenters = ["dc1"]
  type        = "service"

  group "skopos" {
    count = 1 # SQLite has one writer: never run two copies

    network {
      port "http" {
        to = 8080
      }
    }

    # /alloc/data lives on the node's local disk. sticky keeps it when the
    # allocation is replaced on the same node; migrate copies it when Nomad
    # moves the job to another node. When neither applies (the node died),
    # the restore task below rebuilds it from the replica.
    ephemeral_disk {
      sticky  = true
      migrate = true
      size    = 2048 # MB: the database plus the code indexes
    }

    # The shared volume holds only Litestream's replica (plain files, no
    # SQLite locking). Replace with however your cluster mounts it.
    volume "skopos-replica" {
      type            = "csi"
      source          = "skopos-replica"
      access_mode     = "multi-node-multi-writer"
      attachment_mode = "file-system"
    }

    # Before skopos starts: keep a local database that survived on this disk,
    # otherwise restore it from the replica. The first time only, with no
    # replica yet, seed it from a database left on the share by the old
    # setup, then rename that file so it is never used again.
    task "restore" {
      driver = "docker"
      user   = "1000:1000" # the skopos image's user: it must own the files

      lifecycle {
        hook    = "prestart"
        sidecar = false
      }

      config {
        image      = "litestream/litestream:0.3.13"
        entrypoint = ["/bin/sh", "-c"]
        args = [<<-EOS
          set -e
          DB=/alloc/data/skopos.db
          if [ -f "$DB" ]; then
            echo "local database kept"
            exit 0
          fi
          litestream restore -config /local/litestream.yml -if-replica-exists "$DB"
          if [ -f "$DB" ]; then
            echo "restored from the replica"
            exit 0
          fi
          if [ -f /replica/skopos.db ]; then
            echo "no replica yet: seeding from /replica/skopos.db"
            cp /replica/skopos.db "$DB"
            if [ -f /replica/skopos.db-wal ]; then cp /replica/skopos.db-wal "$DB-wal"; fi
            for f in skopos.db skopos.db-wal skopos.db-shm; do
              if [ -f "/replica/$f" ]; then mv "/replica/$f" "/replica/$f.migrated"; fi
            done
          else
            echo "no database anywhere: skopos starts empty"
          fi
          exit 0
          EOS
        ]
      }

      volume_mount {
        volume      = "skopos-replica"
        destination = "/replica"
      }

      template {
        destination = "local/litestream.yml"
        data        = <<-EOF
          dbs:
            - path: /alloc/data/skopos.db
              replicas:
                - type: file
                  path: /replica/litestream/skopos
                  retention: 168h
        EOF
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }

    task "skopos" {
      driver = "docker"
      leader = true # stopping skopos stops Litestream after its final sync

      config {
        image = "skopos:latest"
        ports = ["http"]
      }

      env {
        DATABASE_PATH = "/alloc/data/skopos.db"
        # Code indexes are rebuilt by `skopos index push`: local disk, not
        # replicated.
        SKOPOS_INDEX_DIR = "/alloc/data/indexes"
        LOG_LEVEL        = "info"
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

    # Streams every commit to the replica (about a second behind).
    task "litestream" {
      driver = "docker"
      user   = "1000:1000"

      lifecycle {
        hook    = "poststart"
        sidecar = true
      }

      config {
        image = "litestream/litestream:0.3.13"
        args  = ["replicate", "-config", "/local/litestream.yml"]
      }

      volume_mount {
        volume      = "skopos-replica"
        destination = "/replica"
      }

      template {
        destination = "local/litestream.yml"
        data        = <<-EOF
          dbs:
            - path: /alloc/data/skopos.db
              replicas:
                - type: file
                  path: /replica/litestream/skopos
                  retention: 168h
        EOF
      }

      resources {
        cpu    = 100
        memory = 64
      }
    }
  }
}
