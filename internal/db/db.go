package db

import (
	"database/sql"
	"embed"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fortix/go-libs/netx/dns"
	"github.com/paularlott/logger"
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// DBAddr is a resolved host:port pair, produced by ResolveHost.
type DBAddr struct {
	Host string
	Port int
}

// ResolveHost resolves a host string to a network address.
// If the host starts with "_", it is treated as an SRV name and resolved via DNS.
// Otherwise, it is parsed as host:port (e.g., "localhost:5432").
func ResolveHost(log logger.Logger, host string) (*DBAddr, error) {
	if strings.HasPrefix(host, "_") {
		resolver := dns.NewDNSResolver(dns.ResolverConfig{Logger: log})
		addrs, err := resolver.LookupSRV(host)
		if err != nil {
			return nil, fmt.Errorf("SRV lookup failed for %s: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("no addresses found for SRV %s", host)
		}
		return &DBAddr{
			Host: addrs[0].IP.String(),
			Port: addrs[0].Port,
		}, nil
	}

	h, p, err := net.SplitHostPort(host)
	if err != nil {
		return nil, fmt.Errorf("invalid host:port format %s: %w", host, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return nil, fmt.Errorf("invalid port %s: %w", p, err)
	}
	return &DBAddr{Host: h, Port: port}, nil
}

// sqliteDSN builds a SQLite connection string with pragmas applied per-connection.
// foreign_keys must be in the DSN (not a separate PRAGMA call) so that every
// pooled connection enforces it, otherwise CASCADE deletes silently no-op.
func sqliteDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
}

// Connect opens the SQLite database at path and applies connection-pool tuning.
func Connect(log logger.Logger, path string) (*sql.DB, error) {
	sqlDB, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	// Cap the pool so SQLite write contention is bounded; WAL allows readers to
	// proceed concurrently while busy_timeout serialises writers.
	conns := runtime.NumCPU()
	if conns < 4 {
		conns = 4
	}
	sqlDB.SetMaxOpenConns(conns)
	sqlDB.SetMaxIdleConns(conns)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	log.Info("connected to database", "path", path)
	return sqlDB, nil
}

// RunMigrations executes the embedded schema. Statements use CREATE ... IF NOT
// EXISTS, so this is safe to run on every start.
func RunMigrations(db *sql.DB) error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("reading schema: %w", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		return fmt.Errorf("executing schema: %w", err)
	}
	return nil
}
