package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/paularlott/cli"
	logslog "github.com/paularlott/logger/slog"

	"github.com/martinsuchenak/skopos/cmd/mcp"
	"github.com/martinsuchenak/skopos/cmd/routes"
	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/cleanup"
	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/fleetfile"
	"github.com/martinsuchenak/skopos/internal/health"
	"github.com/martinsuchenak/skopos/internal/idempotency"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/integrations"
	"github.com/martinsuchenak/skopos/internal/itemstate"
	"github.com/martinsuchenak/skopos/internal/itemtext"
	"github.com/martinsuchenak/skopos/internal/notify"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/martinsuchenak/skopos/internal/qa"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/runs"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
	appweb "github.com/martinsuchenak/skopos/web"
	// go-scaffolder:serve-imports
)

func init() {
	Register(serveCmd())
}

func serveCmd() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "Start the skopos service",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:         "server-host",
				DefaultValue: "127.0.0.1",
				Usage:        "Server listen host (set 0.0.0.0 to listen on all interfaces)",
				ConfigPath:   []string{"server.host"},
				EnvVars:      []string{"SERVER_HOST"},
			},
			&cli.IntFlag{
				Name:         "server-port",
				DefaultValue: 8080,
				Usage:        "Server listen port (HTTP, REST, dashboard, and MCP at /mcp)",
				ConfigPath:   []string{"server.port"},
				EnvVars:      []string{"SERVER_PORT"},
			},
			&cli.StringFlag{
				Name:         "database-path",
				DefaultValue: "skopos.db",
				Usage:        "SQLite database path",
				ConfigPath:   []string{"database.path"},
				EnvVars:      []string{"DATABASE_PATH"},
			},
			&cli.StringFlag{
				Name:       "api-key",
				Usage:      "API key required for all API endpoints (reads, writes, MCP, SSE); empty disables auth (loopback binds only, unless --insecure-no-api-key)",
				ConfigPath: []string{"auth.api_key"},
				EnvVars:    []string{"SKOPOS_API_KEY"},
			},
			&cli.BoolFlag{
				Name:    "insecure-no-api-key",
				Usage:   "Allow starting without an api_key on a non-loopback interface (every endpoint is open to the network)",
				EnvVars: []string{"SKOPOS_INSECURE_NO_API_KEY"},
			},
			&cli.IntFlag{
				Name:         "health-stuck-threshold",
				DefaultValue: 15,
				Usage:        "Minutes before an active agent is marked stuck (0 to disable)",
				ConfigPath:   []string{"health.stuck_threshold_minutes"},
				EnvVars:      []string{"HEALTH_STUCK_THRESHOLD"},
			},
			&cli.StringFlag{
				Name:         "index-dir",
				DefaultValue: ".skopos/indexes",
				Usage:        "Directory for per-workspace code index databases",
				ConfigPath:   []string{"codeindex.dir"},
				EnvVars:      []string{"SKOPOS_INDEX_DIR"},
			},
			&cli.StringFlag{
				Name:       "embeddings-url",
				Usage:      "OpenAI-compatible /v1 embeddings endpoint for semantic code search (empty disables; local Ollama e.g. http://localhost:11434/v1)",
				ConfigPath: []string{"codeindex.embeddings.url"},
				EnvVars:    []string{"SKOPOS_EMBEDDINGS_URL"},
			},
			&cli.StringFlag{
				Name:       "embeddings-provider",
				Usage:      "Who runs the embeddings endpoint, for workspace runner policies (default: derived from the URL; a model on another machine on your network is \"local\")",
				ConfigPath: []string{"codeindex.embeddings.provider"},
				EnvVars:    []string{"SKOPOS_EMBEDDINGS_PROVIDER"},
			},
			&cli.StringFlag{
				Name:       "embeddings-model",
				Usage:      "Embedding model name (required with --embeddings-url)",
				ConfigPath: []string{"codeindex.embeddings.model"},
				EnvVars:    []string{"SKOPOS_EMBEDDINGS_MODEL"},
			},
			&cli.StringFlag{
				Name:       "embeddings-api-key",
				Usage:      "API key for the embeddings endpoint (not needed for local servers)",
				ConfigPath: []string{"codeindex.embeddings.api_key"},
				EnvVars:    []string{"SKOPOS_EMBEDDINGS_API_KEY"},
			},
			&cli.StringFlag{
				Name:         "vector-store",
				DefaultValue: "sqlite",
				Usage:        "Vector backend for embeddings: sqlite (embedded, brute force) or qdrant (external, monorepo scale)",
				ConfigPath:   []string{"codeindex.embeddings.vector_store"},
				EnvVars:      []string{"SKOPOS_VECTOR_STORE"},
			},
			&cli.StringFlag{
				Name:       "qdrant-url",
				Usage:      "Qdrant REST address (e.g. http://localhost:6333 or https://qdrant.example.com) when --vector-store=qdrant",
				ConfigPath: []string{"codeindex.embeddings.qdrant_url"},
				EnvVars:    []string{"SKOPOS_QDRANT_URL"},
			},
			&cli.StringFlag{
				Name:       "qdrant-api-key",
				Usage:      "Qdrant API key (when required)",
				ConfigPath: []string{"codeindex.embeddings.qdrant_api_key"},
				EnvVars:    []string{"SKOPOS_QDRANT_API_KEY"},
			},
			&cli.BoolFlag{
				Name:       "mcp-lean-tools",
				Usage:      "Hide non-core MCP tools from tools/list behind tool_search (halves per-step schema weight; tools stay callable via tool_search/execute_tool)",
				ConfigPath: []string{"mcp.lean_tools"},
				EnvVars:    []string{"SKOPOS_MCP_LEAN_TOOLS"},
			},
			&cli.StringFlag{
				Name:         "refresh-interval",
				Usage:        "Poll registered git_url workspaces for refresh every interval (e.g. 30m, 1h; 0 disables)",
				ConfigPath:   []string{"codeindex.refresh_interval"},
				EnvVars:      []string{"SKOPOS_REFRESH_INTERVAL"},
				DefaultValue: "0",
			},
			&cli.StringSliceFlag{
				Name:  "run-stage",
				Usage: "DEPRECATED (imported once into the dashboard pipeline settings; removed next release). Per-workspace stage routing, <workspace>:<stage>=<runner> (repeatable). plan/review/qa take a list of [focus@]runner members to run several agents (e.g. review=security@claude,tests@opencode-glm); compare/synthesize add a judge and a merge after a multi-agent plan round",
			},
			&cli.StringSliceFlag{
				Name:  "stage-policy",
				Usage: "DEPRECATED (imported once into the dashboard pipeline settings; removed next release). How a multi-agent stage is decided, <workspace>:<stage>=<any-blocks|majority|all-pass>[,early-exit][,timeout=60m] (repeatable)",
			},
			&cli.StringSliceFlag{
				Name:  "agents-default",
				Usage: "DEPRECATED (imported once into the dashboard pipeline settings; removed next release). What items without their own agents switch get on a multi-agent stage, <workspace>=<panel|solo> (repeatable; default panel)",
			},
			&cli.IntFlag{
				Name:         "cleanup-retention-days",
				DefaultValue: 30,
				Usage:        "Days to retain data before automatic cleanup (0 to disable)",
				ConfigPath:   []string{"cleanup.retention_days"},
				EnvVars:      []string{"CLEANUP_RETENTION_DAYS"},
			},
			&cli.StringFlag{
				Name:       "integrations-key",
				Usage:      "Key that encrypts webhook secrets; without it webhooks are off (the event feed still works). Keep it: changing it makes stored secrets unreadable",
				ConfigPath: []string{"integrations.secret_key"},
				EnvVars:    []string{"SKOPOS_INTEGRATIONS_KEY"},
			},
			&cli.StringFlag{
				Name:       "public-url",
				Usage:      "The URL other systems reach this server at (e.g. https://skopos.example.com); webhook payloads link to the item from it",
				ConfigPath: []string{"server.public_url"},
				EnvVars:    []string{"SKOPOS_PUBLIC_URL"},
			},
			&cli.StringSliceFlag{
				Name:       "webhooks-allow-private",
				Usage:      "CIDRs (or addresses) webhooks may reach although private, loopback or link-local, e.g. 10.20.0.0/16",
				ConfigPath: []string{"integrations.allow_private"},
				EnvVars:    []string{"SKOPOS_WEBHOOKS_ALLOW_PRIVATE"},
			},
			&cli.StringSliceFlag{
				Name:       "webhooks-allow-http",
				Usage:      "Hosts webhooks may reach over plain http (otherwise https only)",
				ConfigPath: []string{"integrations.allow_http"},
				EnvVars:    []string{"SKOPOS_WEBHOOKS_ALLOW_HTTP"},
			},
		},
		// go-scaffolder:serve-flags
		Run: func(ctx context.Context, cmd *cli.Command) error {
			ship, err := openLogOutput(logOutputFrom(cmd, nil))
			if err != nil {
				return err
			}
			defer closeLogOutput(ship)
			out, format := logWriter(ship, cmd.GetString("log-format"))
			log := logslog.New(logslog.Config{
				Level:  cmd.GetString("log-level"),
				Format: format,
				Writer: out,
			})
			log.Info("starting skopos service")

			if err := appweb.VerifyAssets(); err != nil {
				return err
			}

			apiKey := cmd.GetString("api-key")
			if apiKey == "" {
				if !isLoopbackHost(cmd.GetString("server-host")) && !cmd.GetBool("insecure-no-api-key") {
					return fmt.Errorf("refusing to start without an api_key on a non-loopback interface: set --api-key / SKOPOS_API_KEY, or pass --insecure-no-api-key to explicitly accept that every endpoint is open to the network")
				}
				log.Warn("no api_key configured: authentication is disabled (all endpoints are open)")
			}

			rest.SetLogger(log)

			sqlDB, err := db.Connect(log, cmd.GetString("database-path"))
			if err != nil {
				return err
			}
			defer func() { _ = sqlDB.Close() }()
			if err := db.RunMigrations(sqlDB); err != nil {
				return err
			}
			// SQLite on NFS/SMB once stalled every request for 15-20s (each
			// commit a network round trip) and its WAL mode can corrupt there.
			if fs := db.NetworkFilesystem(cmd.GetString("database-path")); fs != "" {
				log.Warn("the database looks like it is on a network filesystem: expect slow writes and a risk of corruption; keep it on local disk (see docs/deployment/nomad.md for replicating it with Litestream)",
					"path", cmd.GetString("database-path"), "filesystem", fs)
			}
			if fs := db.NetworkFilesystem(filepath.Join(cmd.GetString("index-dir"), "index.db")); fs != "" {
				log.Warn("the code index directory looks like it is on a network filesystem; the indexes are rebuildable, keep them on local disk",
					"path", cmd.GetString("index-dir"), "filesystem", fs)
			}

			// Created before the services: mutations publish through it, and
			// key revocation terminates the revoked key's open SSE streams.
			hub := events.NewHub()

			// Resolve the authenticator before any handler: per-handler
			// authorized() re-authenticates with it (defense in depth behind
			// the router middleware) and must resolve DB keys, not just root.
			authn := auth.NewAuthenticator(apiKey, apikeys.NewStorage(sqlDB))
			// A run's agent token authenticates as that run (run tokens).
			authn.SetRunLookup(runs.NewStorage(sqlDB))

			statusService := status.NewService(status.NewStorage(sqlDB))
			statusHandler := status.NewHandler(statusService, authn)

			blackboardService := blackboard.NewService(blackboard.NewStorage(sqlDB))
			blackboardHandler := blackboard.NewHandler(blackboardService, authn)

			plansStorage := plans.NewStorage(sqlDB)
			plansService := plans.NewService(plansStorage)
			plansHandler := plans.NewHandler(plansService, authn)

			// Inbox: workspace-scoped captures of unprocessed work
			// (docs/design/inbox.md), with the agent-pipeline workflow
			// (docs/design/agent-pipeline.md §1) audited through the
			// append-only log (plan 01a0bf7c).
			auditService := audit.NewService(audit.NewStorage(sqlDB))
			approvalsService := approvals.NewService(approvals.NewStorage(sqlDB))
			inboxService := inbox.NewService(inbox.NewStorage(sqlDB))
			inboxService.SetAuditRecorder(auditService)
			inboxService.SetApprovalLog(approvalsService)
			// Questions and answers (tmp/questions-and-answers — design.md):
			// the record the workflow actions answer through; items written
			// before it are imported once.
			qaService := qa.NewService(qa.NewStorage(sqlDB))
			qaService.SetAuditRecorder(auditService)
			inboxService.SetQuestionLog(qaService)
			if n, err := inboxService.ImportQuestions(ctx); err != nil {
				log.Warn("questions import: " + err.Error())
			} else if n > 0 {
				log.Info("imported the questions and answers of existing items", "items", n)
			}
			inboxService.SetPlanRevisions(plansService) // Approve locks the approved revision
			// Review fix 2: amendments unlock content only through the
			// workflow's pause, and plans with in-flight items survive.
			plansService.SetAmendmentGuard(inboxService.RequireAmendable)
			plansService.SetPlannableGuard(inboxService.RequirePlannable)
			plansService.SetDeletionGuard(inboxService.PlanHasWorkflowItem)
			inboxHandler := inbox.NewHandler(inboxService, authn)

			// Completing a plan completes the inbox items it was converted
			// from (registrar pattern; background context, best-effort).
			plansService.SetCompletionHook(func(planID string) {
				_, _ = inboxService.CompleteForPlan(context.Background(), planID)
			})

			workspacesService := workspaces.NewService(workspaces.NewStorage(sqlDB))
			workspacesHandler := workspaces.NewHandler(workspacesService, authn)

			// Session-derived workspaces are auto-registered so they persist
			// in the registry (same contract as the code-index push path).
			statusService.SetWorkspaceRegistrar(func(id string) {
				_, _, _ = workspacesService.Create(context.Background(), workspaces.CreateInput{ID: id})
			})

			// Code index: one SQLite DB per workspace under --index-dir.
			codeIndexStore, err := codeindex.NewStore(cmd.GetString("index-dir"))
			if err != nil {
				return err
			}
			defer codeIndexStore.Close()
			codeIndexService := codeindex.NewService(codeIndexStore)
			codeIndexHandler := codeindex.NewHandler(codeIndexService, authn)
			codeIndexHandler.SetWorkspaceRegistrar(func(id string) {
				// First push registers the workspace so it persists in the registry.
				_, _, _ = workspacesService.Create(context.Background(), workspaces.CreateInput{ID: id})
			})
			// Server-side indexing: clone/pull the registered git_url and rebuild.
			refresher, err := codeindex.NewRefresher(codeIndexStore, cmd.GetString("index-dir"), func(id string) (string, error) {
				ws, err := workspacesService.Get(context.Background(), id)
				if err != nil {
					// Surface as 400 with guidance instead of a bare 500.
					return "", fmt.Errorf("%w: workspace %s is not registered — POST /api/workspaces with a git_url first: %v", codeindex.ErrInvalidInput, id, err)
				}
				return ws.GitURL, nil
			})
			if err != nil {
				return err
			}
			codeIndexHandler.SetRefresher(refresher)

			// Optional semantic embeddings: any OpenAI-compatible endpoint
			// (local Ollama keeps everything on-host). Disabled by default.
			// Optional external vector backend (default: embedded SQLite).
			if vs := cmd.GetString("vector-store"); vs == "qdrant" {
				if cmd.GetString("qdrant-url") == "" {
					return fmt.Errorf("--qdrant-url is required when --vector-store=qdrant")
				}
				qd, err := codeindex.NewQdrantVectorStore(cmd.GetString("qdrant-url"), cmd.GetString("qdrant-api-key"))
				if err != nil {
					return err
				}
				codeIndexService.SetVectorStore(qd)
				log.Info("vector store: qdrant", "url", cmd.GetString("qdrant-url"))
			}
			var (
				embeddingsFacts  *policy.Facts
				embeddingManager *codeindex.EmbeddingManager
				embeddingsPolicy atomic.Pointer[func(context.Context, string) (*policy.Effective, error)]
			)
			if embURL := cmd.GetString("embeddings-url"); embURL != "" && cmd.GetString("embeddings-model") != "" {
				embedder := &codeindex.OpenAIEmbedder{
					BaseURL:   embURL,
					ModelName: cmd.GetString("embeddings-model"),
					APIKey:    cmd.GetString("embeddings-api-key"),
				}
				embedmgr := codeindex.NewEmbeddingManager(codeIndexService, embedder)
				codeIndexHandler.SetEmbeddingManager(embedmgr)
				// The embeddings endpoint receives symbol names, signatures,
				// doc summaries and every semantic query: a workspace's
				// runner policy covers it like a runner (stage "embeddings").
				// Until the policy is wired below, nothing is sent.
				facts, err := embeddingsFactsFor(embURL, embedder.ModelName, cmd.GetString("embeddings-provider"))
				if err != nil {
					return err
				}
				embeddingsFacts = facts
				embeddingManager = embedmgr
				codeIndexService.SetEmbeddingsGate(func(ctx context.Context, ws string) (bool, bool) {
					lookup := embeddingsPolicy.Load()
					if lookup == nil {
						return false, false
					}
					eff, err := (*lookup)(ctx, ws)
					if err != nil {
						return false, false
					}
					return eff.Allows(*embeddingsFacts, policy.StageEmbeddings).Allowed, true
				})
				mcp.SetSemanticSearcher(embedder)
				codeIndexHandler.Embeddings().SetErrorHandler(func(ws string, err error) {
					log.Warn("background embedding pass failed", "workspace", ws, "error", err)
				})
				log.Info("semantic code search enabled", "model", embedder.ModelName, "url", embURL, "vectors", codeIndexService.VectorStoreName())
			}

			// Cancel background work and initiate graceful shutdown on SIGINT/SIGTERM.
			ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			stuckThreshold := cmd.GetInt("health-stuck-threshold")
			if stuckThreshold > 0 {
				health.NewChecker(sqlDB, time.Duration(stuckThreshold)*time.Minute, log, hub).Start(ctx)
			}
			retentionDays := cmd.GetInt("cleanup-retention-days")
			if retentionDays > 0 {
				cleanupRetention := time.Duration(retentionDays) * 24 * time.Hour
				cleanup.NewCleaner(sqlDB, cleanupRetention, log, hub).Start(ctx)
			}

			// Server-side indexing upkeep: poll registered git_url
			// workspaces, and daily remove index content no branch
			// references (extractor-version bumps orphan old blobs).
			if d, err := time.ParseDuration(cmd.GetString("refresh-interval")); err == nil && d > 0 {
				go func() {
					t := time.NewTicker(d)
					defer t.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-t.C:
							wss, err := workspacesService.List(ctx)
							if err != nil {
								continue
							}
							for _, w := range wss {
								if w.GitURL == "" {
									continue
								}
								if err := refresher.Start(ctx, w.ID, ""); err == nil {
									log.Info("scheduled refresh started", "workspace", w.ID)
								}
							}
						}
					}
				}()
			}
			go func() {
				t := time.NewTicker(24 * time.Hour)
				defer t.Stop()
				runGC := func() {
					wss, err := workspacesService.List(ctx)
					if err != nil {
						return
					}
					for _, w := range wss {
						ids, err := codeIndexService.GC(ctx, w.ID)
						if err != nil {
							log.Warn("index gc failed", "workspace", w.ID, "error", err)
							continue
						}
						if len(ids) > 0 {
							if err := codeIndexService.DeleteVectors(ctx, w.ID, ids); err != nil {
								log.Warn("vector gc failed", "workspace", w.ID, "error", err)
							}
							log.Info("index gc", "workspace", w.ID, "reclaimed", len(ids))
						}
					}
				}
				runGC() // also once at startup
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						runGC()
					}
				}
			}()
			// go-scaffolder:serve-init

			// API keys: DB-backed scoped credentials alongside the root key
			// (docs/design/api-keys.md).
			apiKeysStorage := apikeys.NewStorage(sqlDB)
			apiKeysService := apikeys.NewService(apiKeysStorage)
			apiKeysService.SetAuditRecorder(auditService)
			apiKeysHandler := apikeys.NewHandler(apiKeysService, workspacesService)

			// Service-layer event publishing: the mutating service knows the
			// authoritative workspace, so attribution cannot drift from the
			// data. Revoking a key drops its SSE streams.
			statusService.SetPublisher(hub)
			blackboardService.SetPublisher(hub)
			plansService.SetPublisher(hub)
			inboxService.SetPublisher(hub)
			qaService.SetPublisher(hub)
			workspacesService.SetPublisher(hub)
			codeIndexHandler.SetPublisher(hub)
			apiKeysHandler.SetPublisher(hub)
			apiKeysService.SetRevocationNotifier(hub.DropKey)

			// Workspace groups (agent pipeline 1a): scope changes terminate
			// the affected keys' streams, and a newly registered workspace
			// that matches a group pattern widens every key holding it —
			// both re-evaluate scope on reconnect.
			apiKeysService.SetScopeNotifier(func(keyIDs []string) {
				for _, id := range keyIDs {
					hub.DropKey(id)
				}
			})
			workspacesService.SetCreateNotifier(func(wsID string) {
				apiKeysService.OnWorkspaceRegistered(context.Background(), wsID)
			})

			// Run queue (agent-pipeline lean 1b): workers claim stages of an
			// item's lifecycle; chaining moves the item through the workflow
			// via the system transitions.
			runsService := runs.NewService(runs.NewStorage(sqlDB))
			runsService.SetAuditRecorder(auditService)
			// Pipeline settings (which agents run each stage) live in the
			// database, per workspace or group, edited in the dashboard; the
			// queue reads them at every queue. The old --run-stage /
			// --stage-policy / --agents-default flags are imported once.
			pipelineStorage := pipeline.NewStorage(sqlDB)
			pipelineService := pipeline.NewService(pipelineStorage)
			pipelineService.SetAuditSink(auditService)
			pipelineService.SetPublisher(hub)
			// Runner policies are stored as revisions.
			revisions := confrev.NewStore(sqlDB)
			pipelineService.SetRevisions(revisions)
			// Workspace config and runner definitions live on the server
			// too (internal/wsconfig): the claim names the config a run
			// executes under, and every claim answer says which runner
			// definitions are current.
			configService := wsconfig.NewService(sqlDB, revisions, pipelineStorage)
			configService.SetAuditSink(auditService)
			configService.SetPublisher(hub)
			runsService.SetConfigSource(configService.Rev, configService.Digest)
			runsService.SetFleetRev(configService.FleetRev)
			pipelineService.SetRunnerDefs(configService.RunnerFacts)
			if err := pipelineService.DropOrphans(ctx); err != nil {
				log.Warn("pipeline settings: " + err.Error())
			}
			if flagPrefs, prefWarnings := runWorkspacePrefs(cmd.GetStringSlice("run-stage"), cmd.GetStringSlice("stage-policy"), cmd.GetStringSlice("agents-default")); len(flagPrefs) > 0 || len(prefWarnings) > 0 {
				for _, w := range prefWarnings {
					log.Warn("stage routing flags: " + w)
				}
				for _, line := range pipelineService.Import(context.Background(), flagPrefs, func(ws string) error {
					_, _, err := workspacesService.Create(context.Background(), workspaces.CreateInput{ID: ws})
					return err
				}) {
					log.Info(line)
				}
				log.Warn("--run-stage, --stage-policy and --agents-default are deprecated: pipeline settings now live in the dashboard (Workspaces → Pipeline); the flags are imported once and will be removed in the next release")
			}
			// Unregistering a workspace drops its pipeline settings at once.
			workspacesService.SetDeleteNotifier(func(ws string) {
				if err := pipelineService.DropOrphans(context.Background()); err != nil {
					log.Warn("pipeline settings: " + err.Error())
				}
				configService.DropWorkspace(context.Background(), ws)
			})
			// Second opinions: a plain request uses the workspace's default
			// runner from the pipeline settings.
			inboxService.SetSecondOpinionDefault(pipelineService.SecondOpinionRunner)
			inboxService.SetRunnerPolicy(pipelineService.RunnerAllowed)
			// The embeddings endpoint under the policy (see the gate above).
			if embeddingsFacts != nil {
				facts := *embeddingsFacts
				lookup := pipelineService.EffectivePolicy
				embeddingsPolicy.Store(&lookup)
				pipelineService.SetEmbeddingsCheck(func(ctx context.Context, ws string) (bool, policy.Facts) {
					return codeIndexService.Embedded(ctx, ws), facts
				})
				pipelineService.SetPolicyChanged(func(workspaces []string) {
					for _, ws := range workspaces {
						embeddingManager.Enqueue(ws)
					}
				})
				log.Info("embeddings endpoint", "provider", facts.Provider, "endpoint", facts.Endpoint)
			}
			runsService.SetPrefsSource(func(ctx context.Context, ws string) *runs.WorkspacePrefs {
				return pipelineService.Prefs(ctx, ws)
			})
			// Multi-agent stages: the group summary lands in the item, and
			// groups past their deadline are decided with what they have.
			runsService.SetItemAppend(func(ctx context.Context, itemID, heading, body string) error {
				return inboxService.AppendSection(ctx, itemID, heading, body, audit.ViaWorker)
			})
			// A run's questions: recorded, then rendered into its item.
			runsService.SetQuestionAsker(func(ctx context.Context, r *runs.Run, in runs.QuestionsInput) (any, error) {
				qs := make([]itemtext.Question, len(in.Questions))
				for i, q := range in.Questions {
					qs[i] = itemtext.Question{Text: q.Question, Options: q.Options}
				}
				set, err := qaService.Ask(ctx, qa.AskInput{
					WorkspaceID: r.WorkspaceID, ItemID: r.ItemID, RunID: r.ID, Asker: in.Asker, Stage: r.Stage,
					Runner: r.Runner, Label: r.Label(), SubjectKind: in.SubjectKind, Subject: in.Subject,
					Blocking: in.Blocking, Questions: qs,
				})
				if errors.Is(err, qa.ErrInvalidInput) {
					return nil, fmt.Errorf("%w: %v", runs.ErrInvalidInput, err)
				}
				if err != nil || set == nil {
					return set, err
				}
				if err := inboxService.AppendSection(ctx, r.ItemID, set.Heading(), qa.Render(*set), audit.ViaWorker); err != nil {
					return nil, err
				}
				return set, nil
			})
			// The record as a run's prompt reads it (answers marked as used).
			runsService.SetQuestionReader(func(ctx context.Context, r *runs.Run) (any, error) {
				rec, err := qaService.ForRun(ctx, r.ItemID, r.ID)
				if err != nil {
					return nil, err
				}
				return map[string]any{"sets": rec.Sets, "free": rec.Free, "prompt": qa.PromptText(rec)}, nil
			})
			// The item's own panel/solo switch, read at every queue.
			runsService.SetItemAgents(func(ctx context.Context, itemID string) string {
				item, err := inboxService.GetItem(ctx, itemID)
				if err != nil {
					return ""
				}
				return item.Agents
			})
			go runsService.RunSweepLoop(ctx, time.Minute)
			runsService.SetPlanHooks(planRoundStartHook(inboxService, plansService), ensurePlanHook(inboxService, plansService))
			runsService.SetItemTransition(func(ctx context.Context, itemID, to, reason, agentID string) error {
				toStatus := inbox.Status(to)
				if !inbox.IsWorkflowStatus(toStatus) {
					return fmt.Errorf("chained transition %q is not a workflow status", to)
				}
				_, err := inboxService.SystemTransition(ctx, itemID, toStatus, reason, agentID, audit.ViaWorker)
				return err
			})
			inboxService.SetRunQueuer(runsService)
			inboxService.SetRunLister(runsListerAdapter{runs: runsService})
			// Integrations (tmp/integrations — design.md §3): events in the
			// transaction of each change, webhooks, and the event feed.
			allowPrivate, err := integrations.ParseCIDRs(cmd.GetStringSlice("webhooks-allow-private"))
			if err != nil {
				return err
			}
			var allowHTTP []string
			for _, h := range cmd.GetStringSlice("webhooks-allow-http") {
				if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
					allowHTTP = append(allowHTTP, h)
				}
			}
			emitter := integrations.NewEmitter()
			inboxService.SetEventEmitter(emitter)
			runsService.SetEventEmitter(emitter)
			qaService.SetEventEmitter(emitter)
			integrationsService := integrations.NewService(sqlDB, emitter, cmd.GetString("integrations-key"),
				integrations.URLPolicy{AllowHTTP: allowHTTP, AllowPrivate: allowPrivate})
			integrationsService.PublicURL = strings.TrimRight(strings.TrimSpace(cmd.GetString("public-url")), "/")
			integrationsService.SetAuditRecorder(auditService)
			integrationsService.GroupExists = func(ctx context.Context, ref string) string {
				g, err := apiKeysStorage.ResolveGroupRef(ctx, ref)
				if err != nil {
					return ""
				}
				return g.ID
			}
			integrationsService.GroupScope = func(ctx context.Context, groupID string) ([]string, []string, error) {
				g, err := apiKeysStorage.GetGroup(ctx, groupID)
				return g.Members, g.Patterns, err
			}
			if cmd.GetString("integrations-key") == "" {
				log.Info("webhooks off: no integrations key (integrations.secret_key / SKOPOS_INTEGRATIONS_KEY); the event feed still works")
			} else {
				go integrations.NewDispatcher(integrationsService, log).Run(ctx)
			}
			// GET /api/inbox/{id}/state: the item's state for other
			// systems (tmp/integrations — design.md §4).
			inboxHandler.SetStateHandler(itemstate.NewHandler(&itemstate.Service{
				Items: inboxService, Plans: plansService, Runs: runsService, Questions: qaService,
			}).Get)
			runsHandler := runs.NewHandler(runsService)

			// Slack relay: Socket Mode when tokens are configured; absent =
			// disabled. The relay acts as Martin (user-id checked by the
			// provider) with via=slack on every workflow action; outbound
			// gate notifications hook the inbox transitions.
			// SKOPOS_SLACK_DEFAULT_WORKSPACE additionally enables `new:`
			// capture. The enabled/disabled state is ALWAYS logged at boot —
			// a silent skip reads as "relay broken" from the outside.
			botTok, appTok, martinID := os.Getenv("SKOPOS_SLACK_BOT_TOKEN"), os.Getenv("SKOPOS_SLACK_APP_TOKEN"), os.Getenv("SKOPOS_SLACK_USER_ID")
			if missing := []string{}; botTok == "" || appTok == "" || martinID == "" {
				if botTok == "" {
					missing = append(missing, "SKOPOS_SLACK_BOT_TOKEN")
				}
				if appTok == "" {
					missing = append(missing, "SKOPOS_SLACK_APP_TOKEN")
				}
				if martinID == "" {
					missing = append(missing, "SKOPOS_SLACK_USER_ID")
				}
				log.Info("slack relay disabled", "missing", strings.Join(missing, ", "))
			} else {
				captureWS := os.Getenv("SKOPOS_SLACK_DEFAULT_WORKSPACE")
				log.Info("slack relay enabled", "capture_workspace", captureWS)
				relay := &slackRelay{inbox: inboxService, runs: runsService, revisions: plansService, defaultWS: captureWS, log: log, resolveWS: func(selector string) (string, bool) {
					// Exact id or a unique "/suffix" (mail-processor ->
					// github.com/fortix/mail-processor); ambiguous or
					// unknown = not a selector.
					wss, err := workspacesService.List(context.Background())
					if err != nil {
						return "", false
					}
					match := ""
					for _, w := range wss {
						if w.ID == selector || strings.HasSuffix(w.ID, "/"+selector) {
							if match != "" && match != w.ID {
								return "", false
							}
							match = w.ID
						}
					}
					return match, match != ""
				}}
				notifySvc := notify.New(notify.NewSlack(botTok, appTok, martinID, log), relay)
				relay.SetNotifier(notifySvc)
				// Async: a Slack post (and the buffered replies it drains)
				// must never ride a request goroutine (review M3).
				inboxService.SetItemNotifier(inbox.NewAsyncNotifier(ctx, relay, log))
				// A finished second opinion is posted to the item's thread —
				// off the worker's report request (review M3).
				runsService.SetSecondOpinionNotifier(func(_ context.Context, res runs.SecondOpinionResult) {
					go relay.PostSecondOpinion(ctx, res)
				})
				// A worker whose preflight fails (or recovers) — off the
				// claim request.
				runsService.SetWorkerHealthNotifier(func(_ context.Context, h runs.WorkerHealth) {
					go relay.PostWorkerHealth(ctx, h)
				})
				integrationsService.SetDisableNotifier(func(_ context.Context, w integrations.Webhook, reason string) {
					go relay.PostWebhookDisabled(ctx, w, reason)
				})
				go func() { _ = notifySvc.Run(ctx) }()
				go func() { relay.Run(ctx) }()
			}

			webMux := http.NewServeMux()
			apiMux := http.NewServeMux()
			routes.RegisterRoutes(webMux, apiMux, statusHandler, blackboardHandler, plansHandler, inboxHandler, workspacesHandler, codeIndexHandler, apiKeysHandler, runsHandler)
			routes.RegisterPipelineRoutes(apiMux, pipeline.NewHandler(pipelineService, authn))
			fleetService := fleetfile.NewService(apiKeysService, workspacesService, pipelineService, configService)
			fleetService.SetAuditSink(auditService)
			routes.RegisterConfigRoutes(apiMux, wsconfig.NewHandler(configService, authn), fleetfile.NewHandler(fleetService, authn))
			routes.MountIntegrations(apiMux, integrations.NewHandler(integrationsService))
			mux := http.NewServeMux()
			// Longest-pattern wins: the exact SSE route below overrides /api/.
			// Idempotency-Key covers the inbox's POSTs (integrations retry
			// safely); it runs after auth, since keys are per API key.
			mux.Handle("/api/", authn.Middleware(idempotency.New(sqlDB).Middleware("/api/inbox")(apiMux)))
			mux.Handle("/", webMux)
			mux.Handle("GET /api/events/stream", authn.Middleware(events.StreamHandler(hub)))

			// Runtime metrics are not part of the product API: require authentication
			// when enabled (the middleware is a no-op otherwise).
			mux.Handle("GET /metrics", authn.Middleware(http.HandlerFunc(routes.MetricsHandler)))

			// MCP endpoint, mounted on the same server/port as everything else. Body
			// is capped like the REST API (rest.DecodeJSON applies its cap only to
			// handlers that decode via it).
			if cmd.GetBool("mcp-lean-tools") {
				mcp.SetLeanToolset(true)
			}
			mcp.SetPipelineReader(pipelineService)
			mcpHandler := mcp.NewMCPHandler(statusService, blackboardService, plansService, inboxService, codeIndexService, workspacesService)
			mcpHandler = rest.BodyLimit(noBrowserOrigin(mcpHandler))
			mcpHandler = authn.Middleware(mcpHandler)
			for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions} {
				mux.Handle(m+" /mcp", mcpHandler)
			}

			httpServer := &http.Server{
				Addr:              fmt.Sprintf("%s:%d", cmd.GetString("server-host"), cmd.GetInt("server-port")),
				Handler:           hostAllowed(cmd.GetString("server-host"))(events.MiddlewareWithDBStats(log, sqlDB.Stats, mux)),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second, // SSE handler clears this per-request
				IdleTimeout:       120 * time.Second,
			}
			// Closing the hub ends open SSE streams, so Shutdown() can complete
			// promptly instead of waiting out its timeout on live connections.
			httpServer.RegisterOnShutdown(hub.Close)

			httpErr := make(chan error, 1)
			go func() {
				log.Info("starting HTTP server", "addr", httpServer.Addr, "mcp", "/mcp")
				if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					httpErr <- err
				}
			}()

			select {
			case <-ctx.Done():
				log.Info("shutdown signal received")
			case err := <-httpErr:
				stop()
				return fmt.Errorf("http server: %w", err)
			}

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := httpServer.Shutdown(shutdownCtx); err != nil {
				log.Error("http shutdown error", "error", err)
			}
			return nil
		},
	}
}

// noBrowserOrigin rejects cross-origin MCP requests: real MCP clients never
// send an Origin header, while browsers always do. Without this, the MCP
// handler's unconditional "Access-Control-Allow-Origin: *" turns any website
// in the victim's browser into an MCP client against unauthenticated local
// instances. Preflight OPTIONS requests are denied as well, so the browser
// blocks the call before it reaches the MCP handler.
func noBrowserOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "cross-origin MCP requests are not permitted", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether host is "localhost" or a loopback IP, i.e.
// the server is not reachable from other machines.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostAllowed returns middleware that validates the Host header of every
// request when the server is bound to a loopback address. Browsers treat a
// DNS name that resolves to 127.0.0.1 as same-origin with a loopback service
// (DNS rebinding), so without this check any visited website could read all
// API responses through such a name. Non-loopback binds are not restricted:
// clients legitimately reach those by arbitrary hostnames or IPs.
func hostAllowed(serverHost string) func(http.Handler) http.Handler {
	enforce := isLoopbackHost(serverHost)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if enforce {
				host := strings.ToLower(r.Host)
				if h, _, err := net.SplitHostPort(host); err == nil {
					host = h
				}
				if !isLoopbackHost(host) {
					http.Error(w, "host not allowed", http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// runWorkspacePrefs parses --run-stage <workspace>:<stage>=<members> and
// --stage-policy flags into per-workspace routing. A member is
// [focus@]runner; one member is today's single-runner routing, two or more
// on plan/review/qa make a multi-agent stage (opt-in). --agents-default
// <workspace>=panel|solo is what items without their own switch get there.
// Malformed entries are skipped with a warning rather than failing the
// server.
func runWorkspacePrefs(stageSpecs, policySpecs, agentsSpecs []string) (map[string]*runs.WorkspacePrefs, []string) {
	prefs := map[string]*runs.WorkspacePrefs{}
	var warnings []string
	get := func(ws string) *runs.WorkspacePrefs {
		if prefs[ws] == nil {
			prefs[ws] = &runs.WorkspacePrefs{Stages: map[string]string{}, Groups: map[string][]runs.Member{}, Policies: map[string]runs.StagePolicy{}}
		}
		return prefs[ws]
	}
	for _, spec := range stageSpecs {
		ws, stage, value, ok := splitStageSpec(spec)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("ignoring --run-stage %q: want <workspace>:<stage>=<runner>", spec))
			continue
		}
		var members []runs.Member
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			focus, runner, hasFocus := strings.Cut(part, "@")
			if !hasFocus {
				focus, runner = "", part
			}
			if runner == "" || (hasFocus && !validFocusName(focus)) {
				warnings = append(warnings, fmt.Sprintf("ignoring member %q in --run-stage %q: want [focus@]runner, focus = lowercase letters, digits, - or _", part, spec))
				continue
			}
			members = append(members, runs.Member{Runner: runner, Focus: focus})
		}
		if len(members) == 0 {
			continue
		}
		p := get(ws)
		multiOK := stage == runs.StagePlan || stage == runs.StageReview || stage == runs.StageQA
		if len(members) > 1 && !multiOK {
			warnings = append(warnings, fmt.Sprintf("--run-stage %q: only plan, review and qa run several agents; using %s", spec, members[0].Runner))
			members = members[:1]
		}
		if len(members) == 1 {
			p.Stages[stage] = members[0].Runner
			if members[0].Focus != "" {
				p.Groups[stage] = members // a single focused reviewer
			} else {
				delete(p.Groups, stage)
			}
			continue
		}
		delete(p.Stages, stage)
		p.Groups[stage] = members
	}
	for _, spec := range policySpecs {
		ws, stage, value, ok := splitStageSpec(spec)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("ignoring --stage-policy %q: want <workspace>:<stage>=<policy>[,early-exit][,timeout=60m]", spec))
			continue
		}
		var sp runs.StagePolicy
		bad := false
		for i, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			switch {
			case i == 0 && (part == runs.PolicyAnyBlocks || part == runs.PolicyMajority || part == runs.PolicyAllPass):
				sp.Policy = part
			case part == "early-exit":
				sp.EarlyExit = true
			case strings.HasPrefix(part, "timeout="):
				d, err := time.ParseDuration(strings.TrimPrefix(part, "timeout="))
				if err != nil || d <= 0 {
					bad = true
				}
				sp.Timeout = d
			default:
				bad = true
			}
		}
		if bad || sp.Policy == "" {
			warnings = append(warnings, fmt.Sprintf("ignoring --stage-policy %q: policy is any-blocks, majority or all-pass, then optional early-exit and timeout=<duration>", spec))
			continue
		}
		get(ws).Policies[stage] = sp
	}
	for _, spec := range agentsSpecs {
		ws, value, ok := strings.Cut(spec, "=")
		ws, value = strings.TrimSpace(ws), strings.ToLower(strings.TrimSpace(value))
		if !ok || ws == "" || (value != runs.AgentsPanel && value != runs.AgentsSolo) {
			warnings = append(warnings, fmt.Sprintf("ignoring --agents-default %q: want <workspace>=panel or <workspace>=solo", spec))
			continue
		}
		get(ws).AgentsDefault = value
	}
	return prefs, warnings
}

// planRoundStartHook: a new multi-agent plan round supersedes the previous
// round's open candidates.
func planRoundStartHook(inboxSvc *inbox.Service, plansSvc *plans.Service) func(ctx context.Context, itemID string) error {
	return func(ctx context.Context, itemID string) error {
		item, err := inboxSvc.GetItem(ctx, itemID)
		if err != nil || item.PlanID == "" {
			return nil
		}
		return plansSvc.SupersedeOpenCandidates(ctx, item.PlanID)
	}
}

// ensurePlanHook creates and links the item's plan for a multi-agent plan
// round, so planners only write candidates. Idempotent: when two members
// are claimed at once, the loser's plan is deleted and the linked one kept.
func ensurePlanHook(inboxSvc *inbox.Service, plansSvc *plans.Service) func(ctx context.Context, itemID string) error {
	return func(ctx context.Context, itemID string) error {
		item, err := inboxSvc.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.PlanID != "" {
			return nil
		}
		plan, err := plansSvc.CreatePlan(ctx, plans.CreatePlanInput{WorkspaceID: item.WorkspaceID, Name: item.Title, AuthorAgentID: "skopos"})
		if err != nil {
			return err
		}
		if _, err := inboxSvc.LinkPlan(ctx, itemID, plan.ID); err != nil {
			_ = plansSvc.DeletePlan(ctx, plan.ID)
			if errors.Is(err, inbox.ErrAlreadyConverted) {
				return nil
			}
			return err
		}
		return nil
	}
}

// splitStageSpec splits "<workspace>:<stage>=<value>". The workspace may
// itself contain ':' (host:port), so the stage is taken from the last ':'
// before the '='.
func splitStageSpec(spec string) (ws, stage, value string, ok bool) {
	left, value, found := strings.Cut(spec, "=")
	if !found {
		return "", "", "", false
	}
	i := strings.LastIndex(left, ":")
	if i <= 0 || i == len(left)-1 || strings.TrimSpace(value) == "" {
		return "", "", "", false
	}
	return strings.TrimSpace(left[:i]), strings.TrimSpace(left[i+1:]), strings.TrimSpace(value), true
}

// validFocusName: the focus brief file name (.skopos/focus/<name>.md).
func validFocusName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

type runsListerAdapter struct{ runs *runs.Service }

func (a runsListerAdapter) ListRuns(ctx context.Context, itemID string) ([]inbox.RunSummary, error) {
	rs, err := a.runs.ListRuns(ctx, itemID)
	if err != nil {
		return nil, err
	}
	out := make([]inbox.RunSummary, 0, len(rs))
	for _, r := range rs {
		out = append(out, inbox.RunSummary{ID: r.ID, Stage: r.Stage, Status: r.Status, Runner: r.Runner, Branch: r.Branch, HeadSHA: r.HeadSHA})
	}
	return out, nil
}

// embeddingsFactsFor is what a workspace's runner policy sees of the
// embeddings endpoint: its host and who runs it — declared
// (codeindex.embeddings.provider) or derived from the URL. A declaration
// the URL contradicts is refused: "local" on a URL that has since been
// pointed at a cloud API is exactly what the policy gate is there to catch.
func embeddingsFactsFor(embURL, model, declared string) (*policy.Facts, error) {
	f := &policy.Facts{Name: "embeddings", Kind: "embeddings", Model: model,
		Endpoint: policy.Host(embURL), Provider: policy.ProviderForEndpoint(embURL)}
	if p := strings.ToLower(strings.TrimSpace(declared)); p != "" {
		if policy.Contradicts(p, f.Provider) {
			return nil, fmt.Errorf("codeindex.embeddings.provider is %q, but the embeddings URL %s is %s: fix the declaration or the URL", p, f.Endpoint, f.Provider)
		}
		f.Provider = p
	}
	return f, nil
}
