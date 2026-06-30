package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/paularlott/cli"
	logslog "github.com/paularlott/logger/slog"

	"github.com/martinsuchenak/skopos/internal/cleanup"
	"github.com/martinsuchenak/skopos/internal/db"
)

func init() {
	Register(cleanupCmd())
}

func cleanupCmd() *cli.Command {
	return &cli.Command{
		Name:  "cleanup",
		Usage: "Run a one-time cleanup of old data",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:         "database-path",
				DefaultValue: "skopos.db",
				Usage:        "SQLite database path",
				ConfigPath:   []string{"database.path"},
				EnvVars:      []string{"DATABASE_PATH"},
			},
			&cli.IntFlag{
				Name:         "retention-days",
				DefaultValue: 30,
				Usage:        "Delete data older than this many days",
				ConfigPath:   []string{"cleanup.retention_days"},
				EnvVars:      []string{"CLEANUP_RETENTION_DAYS"},
			},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			log := logslog.New(logslog.Config{
				Level:  cmd.GetString("log-level"),
				Format: cmd.GetString("log-format"),
				Writer: os.Stdout,
			})

		sqlDB, err := db.Connect(log, cmd.GetString("database-path"))
		if err != nil {
			return err
		}
		defer sqlDB.Close()
		if err := db.RunMigrations(sqlDB); err != nil {
			return err
		}

		retention := time.Duration(cmd.GetInt("retention-days")) * 24 * time.Hour
		cleaner := cleanup.NewCleaner(sqlDB, retention, log)
			fmt.Printf("Cleaning up data older than %d days...\n", cmd.GetInt("retention-days"))
			if err := cleaner.RunOnce(ctx); err != nil {
				return err
			}
			fmt.Println("Cleanup complete.")
			return nil
		},
	}
}
