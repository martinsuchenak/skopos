package db

import (
	"database/sql"
	"embed"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/fortix/go-libs/netx/dns"
	"github.com/paularlott/logger"
	_ "modernc.org/sqlite"
	xdaldb "github.com/martinsuchenak/xdal/pkg/sqlite"
	"github.com/martinsuchenak/xdal/pkg/xdal"
)

//go:embed schema.sql
var schemaFS embed.FS

type DBAddr struct {
	Host string
	Port int
}
type Connection struct {
	SQL *sql.DB
	DB  xdal.DBInterface
}

// ResolveHost resolves a host string to a database address.
// If the host starts with "_", it's treated as an SRV name and resolved via DNS.
// Otherwise, it's parsed as host:port (e.g., "localhost:5432").
func ResolveHost(log logger.Logger, host string) (*DBAddr, error) {
	// SRV names start with underscore (e.g., _postgres._tcp.myserver)
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

	// Parse as host:port
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
func Connect(log logger.Logger, host string, user, password, dbName string) (*Connection, error) {
	addr, err := ResolveHost(log, host)
	if err != nil {
		return nil, fmt.Errorf("resolving host: %w", err)
	}
	db, err := sql.Open("sqlite", dbName)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	log.Info("connected to database", "host", addr.Host, "port", addr.Port)
	return &Connection{
		SQL: db,
		DB:  wrapXDALDB(db, log),
	}, nil
}
func wrapXDALDB(sqlDB *sql.DB, log logger.Logger) xdal.DBInterface {
	return xdaldb.NewSQLiteDB(sqlDB, log)
}

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
