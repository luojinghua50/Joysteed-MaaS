package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/bifrostprojection"
	"github.com/luojinghua50/Joysteed-MaaS/internal/bifroststore"
	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/configbus"
	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/luojinghua50/Joysteed-MaaS/internal/fairness"
	"github.com/luojinghua50/Joysteed-MaaS/internal/migrate"
	"github.com/luojinghua50/Joysteed-MaaS/internal/modelaccess"
	"github.com/luojinghua50/Joysteed-MaaS/internal/quota"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rediskv"
	"github.com/luojinghua50/Joysteed-MaaS/internal/requestlog"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rls"
	tenantresolver "github.com/luojinghua50/Joysteed-MaaS/internal/tenantauth"
	"github.com/luojinghua50/Joysteed-MaaS/internal/virtualkey"
	"github.com/luojinghua50/Joysteed-MaaS/plugins/guardrails"
	"github.com/luojinghua50/Joysteed-MaaS/plugins/tenantauth"
	"github.com/luojinghua50/Joysteed-MaaS/plugins/tenantusage"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configtables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/logstore"
	governance "github.com/maximhq/bifrost/plugins/governance"
	loggingplugin "github.com/maximhq/bifrost/plugins/logging"
	"github.com/maximhq/bifrost/transports/bifrost-http/handlers"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	bifrostserver "github.com/maximhq/bifrost/transports/bifrost-http/server"
	"github.com/redis/go-redis/v9"
	"github.com/valyala/fasthttp"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The container build replaces this fallback with Bifrost's compiled UI.
//
//go:embed all:ui
var uiContent embed.FS

var version = "dev"

var _ lib.RuntimeKVStore = (*rediskv.Store)(nil)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		healthcheck()
		return
	}

	level := schemas.LogLevel(strings.ToLower(env("LOG_LEVEL", bifrostserver.DefaultLogLevel)))
	style := schemas.LoggerOutputType(strings.ToLower(env("LOG_STYLE", bifrostserver.DefaultLogOutputStyle)))
	bifrostLogger := bifrost.NewDefaultLogger(level)
	bifrostLogger.SetLevel(level)
	bifrostLogger.SetOutputType(style)
	lib.SetLogger(bifrostLogger)
	bifrostserver.SetLogger(bifrostLogger)
	handlers.SetLogger(bifrostLogger)

	server := bifrostserver.NewBifrostHTTPServer(version, uiContent)
	server.Host = env("APP_HOST", "0.0.0.0")
	server.Port = env("APP_PORT", "8080")
	server.AppDir = env("APP_DIR", "/app/data")
	server.LogLevel = string(level)
	server.LogOutputStyle = string(style)
	server.ConfigStoreDecorator = func(store configstore.ConfigStore) (configstore.ConfigStore, error) {
		return bifroststore.NewPlatformProviderKeyStore(store)
	}
	server.KVStoreFactory = func() (lib.RuntimeKVStore, error) {
		return rediskv.New(runtimeKVConfig())
	}

	ctx := context.Background()
	started := time.Now()
	if err := server.Bootstrap(ctx); err != nil {
		log.Fatal(fmt.Errorf("bootstrap Bifrost: %w", err))
	}
	closeControlDB, err := wireMaaS(ctx, server)
	if err != nil {
		log.Fatal(err)
	}
	defer closeControlDB()

	log.Printf("maas-gateway ready in %s on %s:%s", time.Since(started).Round(time.Millisecond), server.Host, server.Port)
	if err := server.Start(); err != nil {
		log.Fatal(fmt.Errorf("start maas-gateway: %w", err))
	}
}

func wireMaaS(ctx context.Context, server *bifrostserver.BifrostHTTPServer) (func(), error) {
	if server == nil || server.Config == nil || server.Config.ConfigStore == nil {
		return nil, fmt.Errorf("maas-gateway: Bifrost Postgres config store is required")
	}
	if server.Config.ConfigStore.DB() == nil || server.Config.ConfigStore.DB().Dialector.Name() != "postgres" {
		return nil, fmt.Errorf("maas-gateway: Bifrost config store must use Postgres")
	}

	if envBool("MAAS_RUN_DATA_MIGRATIONS", true) {
		if err := migrate.Run(ctx, server.Config.ConfigStore); err != nil {
			return nil, fmt.Errorf("maas-gateway: apply tenant migrations: %w", err)
		}
		if err := migrate.RunIndexes(ctx, server.Config.ConfigStore); err != nil {
			return nil, fmt.Errorf("maas-gateway: apply tenant indexes: %w", err)
		}
	}
	if err := rls.Enforce(ctx, server.Config.ConfigStore.DB(), migrate.TenantScopedTables()); err != nil {
		return nil, fmt.Errorf("maas-gateway: %w", err)
	}
	if _, ok := server.Config.ConfigStore.(*bifroststore.PlatformProviderKeyStore); !ok {
		return nil, fmt.Errorf("maas-gateway: Bifrost management store decorator was not installed")
	}
	internalToken := env("MAAS_INTERNAL_TOKEN", "development-only-maas-internal-token")
	registerInternalModelCatalog(server, internalToken)
	registerInternalRequestLogs(server, internalToken)

	controlDB, err := openControlDatabase(ctx, env("MAAS_DATABASE_URL", "host=postgres port=5432 user=maas password=maas_password dbname=maas sslmode=disable"))
	if err != nil {
		return nil, err
	}
	closeDB := func() {
		sqlDB, dbErr := controlDB.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	}
	billingStore := billing.NewStore(controlDB)
	modelFilter, err := modelaccess.New(billingStore)
	if err != nil {
		closeDB()
		return nil, fmt.Errorf("maas-gateway: initialize model access filter: %w", err)
	}
	server.ListModelsFilter = modelFilter.FilterListModels

	resolver, err := tenantresolver.NewResolver(server.Config.ConfigStore, controlplane.NewStore(controlDB))
	if err != nil {
		closeDB()
		return nil, fmt.Errorf("maas-gateway: initialize tenant resolver: %w", err)
	}
	authPlugin, err := tenantauth.New(resolver)
	if err != nil {
		closeDB()
		return nil, err
	}
	preBuiltin := schemas.PluginPlacementPreBuiltin
	authOrder := -100
	if err := server.SyncLoadedPlugin(ctx, authPlugin.GetName(), authPlugin, &preBuiltin, &authOrder); err != nil {
		closeDB()
		return nil, fmt.Errorf("maas-gateway: register tenant authentication: %w", err)
	}

	if blocked := splitCSV(os.Getenv("MAAS_GUARDRAIL_BLOCKED_TERMS")); len(blocked) > 0 {
		checker := guardrails.KeywordChecker(blocked...)
		plugin := guardrails.New(guardrails.Config{CheckInput: checker, CheckOutput: checker})
		postBuiltin := schemas.PluginPlacementPostBuiltin
		guardrailOrder := 100
		if err := server.SyncLoadedPlugin(ctx, plugin.GetName(), plugin, &postBuiltin, &guardrailOrder); err != nil {
			closeDB()
			return nil, fmt.Errorf("maas-gateway: register guardrails: %w", err)
		}
	}

	keyCipher, err := virtualkey.NewCipher(env("MAAS_KEY_ENCRYPTION_KEY", "development-only-maas-key-encryption-material"))
	if err != nil {
		closeDB()
		return nil, err
	}
	keyStore, err := virtualkey.NewStore(controlDB, keyCipher)
	if err != nil {
		closeDB()
		return nil, err
	}
	if err := keyStore.Migrate(ctx); err != nil {
		closeDB()
		return nil, fmt.Errorf("maas-gateway: migrate key projection state: %w", err)
	}
	governancePlugin, err := lib.FindPluginAs[governance.BaseGovernancePlugin](server.Config, governance.PluginName)
	if err != nil {
		closeDB()
		return nil, fmt.Errorf("maas-gateway: locate governance runtime: %w", err)
	}
	target, err := bifrostprojection.New(server.Config.ConfigStore, governanceRuntime{store: governancePlugin.GetGovernanceStore()})
	if err != nil {
		closeDB()
		return nil, err
	}
	projector, err := virtualkey.NewProjector(controlDB, keyCipher, target)
	if err != nil {
		closeDB()
		return nil, err
	}
	bus := configbus.NewStore(controlDB)
	reconciler, err := configbus.NewReconciler(bus, projector.ProjectTenant)
	if err != nil {
		closeDB()
		return nil, err
	}
	projectionCtx, cancelProjection := context.WithCancel(ctx)
	if err := reconciler.Reconcile(projectionCtx); err != nil {
		log.Printf("initial virtual-key reconciliation will be retried: %v", err)
	}
	interval := envDuration("MAAS_CONFIG_RECONCILE_INTERVAL", 15*time.Second)
	go func() {
		if err := reconciler.Run(projectionCtx, interval, func(err error) {
			log.Printf("virtual-key reconciliation failed: %v", err)
		}); err != nil && projectionCtx.Err() == nil {
			log.Printf("virtual-key reconciler stopped: %v", err)
		}
	}()

	var redisClient *redis.Client
	if addr := env("MAAS_REDIS_ADDR", "redis:6379"); addr != "" {
		redisClient = redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("MAAS_REDIS_PASSWORD")})
		notifier, notifyErr := configbus.NewRedisNotifier(redisClient, configbus.DefaultChannel)
		if notifyErr == nil {
			notifications, subscriptionErrors, subscribeErr := notifier.Subscribe(projectionCtx)
			if subscribeErr != nil {
				log.Printf("Redis config notifications unavailable; periodic reconciliation remains active: %v", subscribeErr)
			} else {
				go consumeConfigNotifications(projectionCtx, reconciler, notifications, subscriptionErrors)
			}
		}
	}
	if envBool("MAAS_USAGE_ENFORCEMENT", true) {
		if redisClient == nil {
			cancelProjection()
			closeDB()
			return nil, fmt.Errorf("maas-gateway: Redis is required for usage enforcement")
		}
		semaphore, semaphoreErr := fairness.NewSemaphore(redisClient, "")
		if semaphoreErr != nil {
			cancelProjection()
			closeDB()
			return nil, semaphoreErr
		}
		counter, counterErr := quota.NewCounter(redisClient, "")
		if counterErr != nil {
			cancelProjection()
			closeDB()
			return nil, counterErr
		}
		usagePlugin, usageErr := tenantusage.New(tenantusage.Config{
			Billing: billingStore, Semaphore: semaphore, Counter: counter,
			LeaseTTL:              envDuration("MAAS_CONCURRENCY_LEASE_TTL", 5*time.Minute),
			ProviderMaxConcurrent: envInt("MAAS_PROVIDER_MAX_CONCURRENT", 0),
			MarkupBasisPoints:     int64(envInt("MAAS_BILLING_MARKUP_BPS", 1000)),
		})
		if usageErr != nil {
			cancelProjection()
			closeDB()
			return nil, usageErr
		}
		postBuiltin := schemas.PluginPlacementPostBuiltin
		usageOrder := -100
		if err := server.SyncLoadedPlugin(ctx, usagePlugin.GetName(), usagePlugin, &postBuiltin, &usageOrder); err != nil {
			cancelProjection()
			closeDB()
			return nil, fmt.Errorf("maas-gateway: register tenant usage plugin: %w", err)
		}
	}

	cleanup := func() {
		cancelProjection()
		if redisClient != nil {
			_ = redisClient.Close()
		}
		closeDB()
	}
	return cleanup, nil
}

type catalogModel struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// registerInternalModelCatalog exposes only model identities to the control
// plane. It deliberately does not reuse Dashboard authentication or serialize
// provider configuration, which contains credentials.
func registerInternalModelCatalog(server *bifrostserver.BifrostHTTPServer, token string) {
	server.Router.GET("/maas/internal/models", func(ctx *fasthttp.RequestCtx) {
		provided := string(ctx.Request.Header.Peek("X-MaaS-Internal-Token"))
		if token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			ctx.SetStatusCode(fasthttp.StatusUnauthorized)
			handlers.SendJSON(ctx, map[string]string{"error": "unauthorized"})
			return
		}
		catalog := server.Config.GetModelCatalog()
		providers, err := server.Config.GetAllProviders()
		if err != nil || catalog == nil {
			ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
			handlers.SendJSON(ctx, map[string]string{"error": "model catalog unavailable"})
			return
		}
		models := make([]catalogModel, 0)
		seen := make(map[string]struct{})
		for _, provider := range providers {
			for _, model := range catalog.GetModelsForProvider(provider) {
				model = strings.TrimSpace(model)
				if model == "" {
					continue
				}
				id := string(provider) + "/" + model
				if _, exists := seen[id]; exists {
					continue
				}
				seen[id] = struct{}{}
				models = append(models, catalogModel{ID: id, Provider: string(provider), Model: model})
			}
		}
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		handlers.SendJSON(ctx, map[string]any{"models": models, "total": len(models)})
	})
}

func registerInternalRequestLogs(server *bifrostserver.BifrostHTTPServer, token string) {
	authorized := func(ctx *fasthttp.RequestCtx) bool {
		provided := string(ctx.Request.Header.Peek("X-MaaS-Internal-Token"))
		if token != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
			return true
		}
		ctx.SetStatusCode(fasthttp.StatusUnauthorized)
		handlers.SendJSON(ctx, map[string]string{"error": "unauthorized"})
		return false
	}
	keyIDs := func(ctx *fasthttp.RequestCtx) []string {
		seen := make(map[string]struct{})
		values := make([]string, 0)
		ctx.QueryArgs().VisitAll(func(key, value []byte) {
			if string(key) != "virtual_key_id" {
				return
			}
			for _, candidate := range strings.Split(string(value), ",") {
				candidate = strings.TrimSpace(candidate)
				if candidate == "" {
					continue
				}
				if _, exists := seen[candidate]; exists {
					continue
				}
				seen[candidate] = struct{}{}
				values = append(values, candidate)
			}
		})
		return values
	}
	manager := func(ctx *fasthttp.RequestCtx) *loggingplugin.PluginLogManager {
		plugin, err := lib.FindPluginAs[*loggingplugin.LoggerPlugin](server.Config, loggingplugin.PluginName)
		if err != nil || plugin == nil {
			ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
			handlers.SendJSON(ctx, map[string]string{"error": "request log store unavailable"})
			return nil
		}
		return plugin.GetPluginLogManager()
	}

	server.Router.GET("/maas/internal/logs", func(ctx *fasthttp.RequestCtx) {
		if !authorized(ctx) {
			return
		}
		ids := keyIDs(ctx)
		if len(ids) == 0 {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			handlers.SendJSON(ctx, map[string]string{"error": "virtual_key_id is required"})
			return
		}
		limit, err := strconv.Atoi(string(ctx.QueryArgs().Peek("limit")))
		if err != nil || limit <= 0 {
			limit = 50
		}
		if limit > 100 {
			limit = 100
		}
		offset, err := strconv.Atoi(string(ctx.QueryArgs().Peek("offset")))
		if err != nil || offset < 0 {
			offset = 0
		}
		logManager := manager(ctx)
		if logManager == nil {
			return
		}
		result, err := logManager.Search(ctx, &logstore.SearchFilters{VirtualKeyIDs: ids, RootsOnly: true}, &logstore.PaginationOptions{Limit: limit, Offset: offset, SortBy: "timestamp", Order: "desc"})
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusInternalServerError)
			handlers.SendJSON(ctx, map[string]string{"error": "request log search failed"})
			return
		}
		items := make([]requestlog.ListItem, 0, len(result.Logs))
		for i := range result.Logs {
			items = append(items, requestlog.ProjectList(requestLogSource(&result.Logs[i])))
		}
		handlers.SendJSON(ctx, requestlog.ListResponse{Logs: items, Limit: limit, Offset: offset, TotalCount: result.Pagination.TotalCount})
	})

	server.Router.GET("/maas/internal/logs/{id}", func(ctx *fasthttp.RequestCtx) {
		if !authorized(ctx) {
			return
		}
		ids := keyIDs(ctx)
		if len(ids) == 0 {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			handlers.SendJSON(ctx, map[string]string{"error": "virtual_key_id is required"})
			return
		}
		id, _ := ctx.UserValue("id").(string)
		if strings.TrimSpace(id) == "" {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			handlers.SendJSON(ctx, map[string]string{"error": "request log not found"})
			return
		}
		logManager := manager(ctx)
		if logManager == nil {
			return
		}
		entry, err := logManager.GetLog(ctx, id)
		allowed := make(map[string]struct{}, len(ids))
		for _, keyID := range ids {
			allowed[keyID] = struct{}{}
		}
		virtualKeyID := ""
		if entry != nil && entry.VirtualKeyID != nil {
			virtualKeyID = *entry.VirtualKeyID
		}
		if err != nil || !requestlog.Owns(virtualKeyID, allowed) {
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			handlers.SendJSON(ctx, map[string]string{"error": "request log not found"})
			return
		}
		handlers.SendJSON(ctx, requestlog.ProjectDetail(requestLogSource(entry)))
	})
}

func requestLogSource(log *logstore.Log) requestlog.Source {
	if log == nil {
		return requestlog.Source{}
	}
	virtualKeyID := ""
	if log.VirtualKeyID != nil {
		virtualKeyID = *log.VirtualKeyID
	}
	return requestlog.Source{
		ID: log.ID, Timestamp: log.Timestamp, Status: log.Status,
		Provider: log.Provider, Model: log.Model, Object: log.Object,
		LatencyMS: log.Latency, TotalTokens: log.TotalTokens, Cost: log.Cost,
		Stream: log.Stream, VirtualKeyID: virtualKeyID,
		ContentHidden: log.ContentHidden,
		Request: map[string]any{
			"messages": log.InputHistoryParsed, "responses_input": log.ResponsesInputHistoryParsed,
			"parameters": log.ParamsParsed, "tools": log.ToolsParsed,
			"speech": log.SpeechInputParsed, "transcription": log.TranscriptionInputParsed,
			"ocr": log.OCRInputParsed, "image_generation": log.ImageGenerationInputParsed,
			"image_edit": log.ImageEditInputParsed, "image_variation": log.ImageVariationInputParsed,
			"video_generation": log.VideoGenerationInputParsed, "video_edit": log.VideoEditInputParsed,
		},
		Response: map[string]any{
			"message": log.OutputMessageParsed, "responses_output": log.ResponsesOutputParsed,
			"embeddings": log.EmbeddingOutputParsed, "rerank": log.RerankOutputParsed,
			"ocr": log.OCROutputParsed, "speech": log.SpeechOutputParsed,
			"transcription": log.TranscriptionOutputParsed, "image_generation": log.ImageGenerationOutputParsed,
			"models": log.ListModelsOutputParsed, "video_generation": log.VideoGenerationOutputParsed,
			"video_retrieve": log.VideoRetrieveOutputParsed, "video_download": log.VideoDownloadOutputParsed,
			"video_list": log.VideoListOutputParsed, "video_delete": log.VideoDeleteOutputParsed,
			"token_usage": log.TokenUsageParsed, "stop_reason": log.StopReason,
		},
		Routing: map[string]any{
			"provider": log.Provider, "model": log.Model, "served_model": log.ServedModel,
			"alias": log.Alias, "routing_engines": log.RoutingEnginesUsed,
			"number_of_retries": log.NumberOfRetries,
			"fallback_index":    log.FallbackIndex, "latency_ms": log.Latency,
			"upstream_latency_ms": log.UpstreamLatency, "overhead_latency_ms": log.OverheadLatency,
		},
		Error: map[string]any{"details": log.ErrorDetailsParsed},
	}
}

type governanceRuntime struct{ store governance.GovernanceStore }

func (r governanceRuntime) UpsertVirtualKey(ctx context.Context, key *configtables.TableVirtualKey) {
	r.store.UpdateVirtualKeyInMemory(ctx, key, nil, nil, nil)
}

func (r governanceRuntime) DeleteVirtualKey(ctx context.Context, keyID string) {
	r.store.DeleteVirtualKeyInMemory(ctx, keyID)
}

func consumeConfigNotifications(ctx context.Context, reconciler *configbus.Reconciler, notifications <-chan configbus.Notification, errs <-chan error) {
	for notifications != nil || errs != nil {
		select {
		case <-ctx.Done():
			return
		case notification, ok := <-notifications:
			if !ok {
				notifications = nil
				continue
			}
			if err := reconciler.Observe(ctx, notification.TenantID, notification.Generation); err != nil {
				log.Printf("notification-triggered virtual-key reconciliation failed: %v", err)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			log.Printf("Redis config notification error: %v", err)
		}
	}
}

func openControlDatabase(ctx context.Context, dsn string) (*gorm.DB, error) {
	var last error
	for attempt := 1; attempt <= 30; attempt++ {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
		if err == nil {
			sqlDB, sqlErr := db.DB()
			if sqlErr == nil {
				pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				pingErr := sqlDB.PingContext(pingCtx)
				cancel()
				if pingErr == nil {
					return db, nil
				}
				last = pingErr
			}
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("maas-gateway: control database unavailable: %w", last)
}

func healthcheck() {
	url := "http://127.0.0.1:" + env("APP_PORT", "8080") + "/health"
	client := http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(url)
	if err != nil || response.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	_ = response.Body.Close()
}

func runtimeKVConfig() *rediskv.Config {
	sharedAddr := env("MAAS_REDIS_ADDR", "redis:6379")
	sharedPassword := os.Getenv("MAAS_REDIS_PASSWORD")
	return &rediskv.Config{
		Addr:               schemas.NewSecretVar(env("MAAS_KV_REDIS_ADDR", sharedAddr)),
		Username:           optionalSecretVar(os.Getenv("MAAS_KV_REDIS_USERNAME")),
		Password:           optionalSecretVar(envOverride("MAAS_KV_REDIS_PASSWORD", sharedPassword)),
		DB:                 optionalSecretVar(os.Getenv("MAAS_KV_REDIS_DB")),
		UseTLS:             optionalBoolSecretVar("MAAS_KV_REDIS_USE_TLS"),
		InsecureSkipVerify: optionalBoolSecretVar("MAAS_KV_REDIS_INSECURE_SKIP_VERIFY"),
		CACertPEM:          optionalSecretVar(os.Getenv("MAAS_KV_REDIS_CA_CERT_PEM")),
		ClusterMode:        optionalBoolSecretVar("MAAS_KV_REDIS_CLUSTER_MODE"),
		OpTimeout:          schemas.Duration(envDuration("MAAS_KV_REDIS_OP_TIMEOUT", 2*time.Second)),
		KeyPrefix:          env("MAAS_KV_REDIS_KEY_PREFIX", rediskv.DefaultKeyPrefix),
	}
}

func envOverride(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func optionalSecretVar(value string) *schemas.SecretVar {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return schemas.NewSecretVar(value)
}

func optionalBoolSecretVar(key string) *schemas.SecretVar {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return nil
	}
	return schemas.NewSecretVar(value)
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			return parsed
		}
		log.Printf("invalid %s=%q; using %s", key, value, fallback)
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed >= 0 {
			return parsed
		}
		log.Printf("invalid %s=%q; using %d", key, value, fallback)
	}
	return fallback
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
