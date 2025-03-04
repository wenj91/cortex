package api

import (
	"context"
	"flag"
	"net/http"
	"path"
	"strings"

	"github.com/NYTimes/gziphandler"
	"github.com/felixge/fgprof"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/prometheus/storage"
	"github.com/weaveworks/common/middleware"
	"github.com/weaveworks/common/server"

	"github.com/cortexproject/cortex/pkg/chunk/purger"
	"github.com/cortexproject/cortex/pkg/compactor"
	"github.com/cortexproject/cortex/pkg/cortexpb"
	"github.com/cortexproject/cortex/pkg/distributor"
	"github.com/cortexproject/cortex/pkg/distributor/distributorpb"
	frontendv1 "github.com/cortexproject/cortex/pkg/frontend/v1"
	"github.com/cortexproject/cortex/pkg/frontend/v1/frontendv1pb"
	frontendv2 "github.com/cortexproject/cortex/pkg/frontend/v2"
	"github.com/cortexproject/cortex/pkg/frontend/v2/frontendv2pb"
	"github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/querier"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/scheduler"
	"github.com/cortexproject/cortex/pkg/scheduler/schedulerpb"
	"github.com/cortexproject/cortex/pkg/storegateway"
	"github.com/cortexproject/cortex/pkg/storegateway/storegatewaypb"
	"github.com/cortexproject/cortex/pkg/util/push"
)

// DistributorPushWrapper wraps around a push. It is similar to middleware.Interface.
type DistributorPushWrapper func(next push.Func) push.Func
type ConfigHandler func(actualCfg interface{}, defaultCfg interface{}) http.HandlerFunc

type Config struct {
	ResponseCompression bool `yaml:"response_compression_enabled"`

	AlertmanagerHTTPPrefix string `yaml:"alertmanager_http_prefix"`
	PrometheusHTTPPrefix   string `yaml:"prometheus_http_prefix"`

	// The following configs are injected by the upstream caller.
	ServerPrefix       string               `yaml:"-"`
	LegacyHTTPPrefix   string               `yaml:"-"`
	HTTPAuthMiddleware middleware.Interface `yaml:"-"`

	// This allows downstream projects to wrap the distributor push function
	// and access the deserialized write requests before/after they are pushed.
	DistributorPushWrapper DistributorPushWrapper `yaml:"-"`

	// The CustomConfigHandler allows for providing a different handler for the
	// `/config` endpoint. If this field is set _before_ the API module is
	// initialized, the custom config handler will be used instead of
	// DefaultConfigHandler.
	CustomConfigHandler ConfigHandler `yaml:"-"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	f.BoolVar(&cfg.ResponseCompression, "api.response-compression-enabled", false, "Use GZIP compression for API responses. Some endpoints serve large YAML or JSON blobs which can benefit from compression.")
	cfg.RegisterFlagsWithPrefix("", f)
}

// RegisterFlagsWithPrefix adds the flags required to config this to the given FlagSet with the set prefix.
func (cfg *Config) RegisterFlagsWithPrefix(prefix string, f *flag.FlagSet) {
	f.StringVar(&cfg.AlertmanagerHTTPPrefix, prefix+"http.alertmanager-http-prefix", "/alertmanager", "HTTP URL path under which the Alertmanager ui and api will be served.")
	f.StringVar(&cfg.PrometheusHTTPPrefix, prefix+"http.prometheus-http-prefix", "/prometheus", "HTTP URL path under which the Prometheus api will be served.")
}

// Push either wraps the distributor push function as configured or returns the distributor push directly.
func (cfg *Config) wrapDistributorPush(d *distributor.Distributor) push.Func {
	if cfg.DistributorPushWrapper != nil {
		return cfg.DistributorPushWrapper(d.Push)
	}

	return d.Push
}

type API struct {
	AuthMiddleware middleware.Interface

	cfg       Config
	server    *server.Server
	logger    log.Logger
	sourceIPs *middleware.SourceIPExtractor
	indexPage *IndexPageContent
}

func New(cfg Config, serverCfg server.Config, s *server.Server, logger log.Logger) (*API, error) {
	// Ensure the encoded path is used. Required for the rules API
	s.HTTP.UseEncodedPath()

	var sourceIPs *middleware.SourceIPExtractor
	if serverCfg.LogSourceIPs {
		var err error
		sourceIPs, err = middleware.NewSourceIPs(serverCfg.LogSourceIPsHeader, serverCfg.LogSourceIPsRegex)
		if err != nil {
			// This should have already been caught in the Server creation
			return nil, err
		}
	}

	api := &API{
		cfg:            cfg,
		AuthMiddleware: cfg.HTTPAuthMiddleware,
		server:         s,
		logger:         logger,
		sourceIPs:      sourceIPs,
		indexPage:      newIndexPageContent(),
	}

	// If no authentication middleware is present in the config, use the default authentication middleware.
	if cfg.HTTPAuthMiddleware == nil {
		api.AuthMiddleware = middleware.AuthenticateUser
	}

	return api, nil
}

// RegisterRoute registers a single route enforcing HTTP methods. A single
// route is expected to be specific about which HTTP methods are supported.
func (a *API) RegisterRoute(path string, handler http.Handler, auth bool, method string, methods ...string) {
	methods = append([]string{method}, methods...)

	level.Debug(a.logger).Log("msg", "api: registering route", "methods", strings.Join(methods, ","), "path", path, "auth", auth)

	if auth {
		handler = a.AuthMiddleware.Wrap(handler)
	}

	if a.cfg.ResponseCompression {
		handler = gziphandler.GzipHandler(handler)
	}

	if len(methods) == 0 {
		a.server.HTTP.Path(path).Handler(handler)
		return
	}
	a.server.HTTP.Path(path).Methods(methods...).Handler(handler)
}

func (a *API) RegisterRoutesWithPrefix(prefix string, handler http.Handler, auth bool, methods ...string) {
	level.Debug(a.logger).Log("msg", "api: registering route", "methods", strings.Join(methods, ","), "prefix", prefix, "auth", auth)
	if auth {
		handler = a.AuthMiddleware.Wrap(handler)
	}

	if a.cfg.ResponseCompression {
		handler = gziphandler.GzipHandler(handler)
	}

	if len(methods) == 0 {
		a.server.HTTP.PathPrefix(prefix).Handler(handler)
		return
	}
	a.server.HTTP.PathPrefix(prefix).Methods(methods...).Handler(handler)
}

// RegisterAPI registers the standard endpoints associated with a running Cortex.
func (a *API) RegisterAPI(httpPathPrefix string, actualCfg interface{}, defaultCfg interface{}) {
	a.indexPage.AddLink(SectionAdminEndpoints, "/config", "Current Config (including the default values)")
	a.indexPage.AddLink(SectionAdminEndpoints, "/config?mode=diff", "Current Config (show only values that differ from the defaults)")

	a.RegisterRoute("/config", a.cfg.configHandler(actualCfg, defaultCfg), false, "GET")
	a.RegisterRoute("/", indexHandler(httpPathPrefix, a.indexPage), false, "GET")
	a.RegisterRoute("/debug/fgprof", fgprof.Handler(), false, "GET")
}

// RegisterRuntimeConfig registers the endpoints associates with the runtime configuration
func (a *API) RegisterRuntimeConfig(runtimeConfigHandler http.HandlerFunc) {
	a.indexPage.AddLink(SectionAdminEndpoints, "/runtime_config", "Current Runtime Config (incl. Overrides)")
	a.indexPage.AddLink(SectionAdminEndpoints, "/runtime_config?mode=diff", "Current Runtime Config (show only values that differ from the defaults)")

	a.RegisterRoute("/runtime_config", runtimeConfigHandler, false, "GET")
}

// RegisterDistributor registers the endpoints associated with the distributor.
func (a *API) RegisterDistributor(d *distributor.Distributor, pushConfig distributor.Config) {
	distributorpb.RegisterDistributorServer(a.server.GRPC, d)

	a.RegisterRoute("/api/v1/push", push.Handler(pushConfig.MaxRecvMsgSize, a.sourceIPs, a.cfg.wrapDistributorPush(d)), true, "POST")

	a.indexPage.AddLink(SectionAdminEndpoints, "/distributor/ring", "Distributor Ring Status")
	a.indexPage.AddLink(SectionAdminEndpoints, "/distributor/all_user_stats", "Usage Statistics")
	a.indexPage.AddLink(SectionAdminEndpoints, "/distributor/ha_tracker", "HA Tracking Status")

	a.RegisterRoute("/distributor/ring", d, false, "GET", "POST")
	a.RegisterRoute("/distributor/all_user_stats", http.HandlerFunc(d.AllUserStatsHandler), false, "GET")
	a.RegisterRoute("/distributor/ha_tracker", d.HATracker, false, "GET")

	// Legacy Routes
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/push"), push.Handler(pushConfig.MaxRecvMsgSize, a.sourceIPs, a.cfg.wrapDistributorPush(d)), true, "POST")
	a.RegisterRoute("/all_user_stats", http.HandlerFunc(d.AllUserStatsHandler), false, "GET")
	a.RegisterRoute("/ha-tracker", d.HATracker, false, "GET")
}

// Ingester is defined as an interface to allow for alternative implementations
// of ingesters to be passed into the API.RegisterIngester() method.
type Ingester interface {
	client.IngesterServer
	FlushHandler(http.ResponseWriter, *http.Request)
	ShutdownHandler(http.ResponseWriter, *http.Request)
	Push(context.Context, *cortexpb.WriteRequest) (*cortexpb.WriteResponse, error)
}

// RegisterIngester registers the ingesters HTTP and GRPC service
func (a *API) RegisterIngester(i Ingester, pushConfig distributor.Config) {
	client.RegisterIngesterServer(a.server.GRPC, i)

	a.indexPage.AddLink(SectionDangerous, "/ingester/flush", "Trigger a Flush of data from Ingester to storage")
	a.indexPage.AddLink(SectionDangerous, "/ingester/shutdown", "Trigger Ingester Shutdown (Dangerous)")
	a.RegisterRoute("/ingester/flush", http.HandlerFunc(i.FlushHandler), false, "GET", "POST")
	a.RegisterRoute("/ingester/shutdown", http.HandlerFunc(i.ShutdownHandler), false, "GET", "POST")
	a.RegisterRoute("/ingester/push", push.Handler(pushConfig.MaxRecvMsgSize, a.sourceIPs, i.Push), true, "POST") // For testing and debugging.

	// Legacy Routes
	a.RegisterRoute("/flush", http.HandlerFunc(i.FlushHandler), false, "GET", "POST")
	a.RegisterRoute("/shutdown", http.HandlerFunc(i.ShutdownHandler), false, "GET", "POST")
	a.RegisterRoute("/push", push.Handler(pushConfig.MaxRecvMsgSize, a.sourceIPs, i.Push), true, "POST") // For testing and debugging.
}

func (a *API) RegisterTenantDeletion(api *purger.TenantDeletionAPI) {
	a.RegisterRoute("/purger/delete_tenant", http.HandlerFunc(api.DeleteTenant), true, "POST")
	a.RegisterRoute("/purger/delete_tenant_status", http.HandlerFunc(api.DeleteTenantStatus), true, "GET")
}

// RegisterRing registers the ring UI page associated with the distributor for writes.
func (a *API) RegisterRing(r *ring.Ring) {
	a.indexPage.AddLink(SectionAdminEndpoints, "/ingester/ring", "Ingester Ring Status")
	a.RegisterRoute("/ingester/ring", r, false, "GET", "POST")

	// Legacy Route
	a.RegisterRoute("/ring", r, false, "GET", "POST")
}

// RegisterStoreGateway registers the ring UI page associated with the store-gateway.
func (a *API) RegisterStoreGateway(s *storegateway.StoreGateway) {
	storegatewaypb.RegisterStoreGatewayServer(a.server.GRPC, s)

	a.indexPage.AddLink(SectionAdminEndpoints, "/store-gateway/ring", "Store Gateway Ring")
	a.RegisterRoute("/store-gateway/ring", http.HandlerFunc(s.RingHandler), false, "GET", "POST")
}

// RegisterCompactor registers the ring UI page associated with the compactor.
func (a *API) RegisterCompactor(c *compactor.Compactor) {
	a.indexPage.AddLink(SectionAdminEndpoints, "/compactor/ring", "Compactor Ring Status")
	a.RegisterRoute("/compactor/ring", http.HandlerFunc(c.RingHandler), false, "GET", "POST")
}

type Distributor interface {
	querier.Distributor
	UserStatsHandler(w http.ResponseWriter, r *http.Request)
}

// RegisterQueryable registers the the default routes associated with the querier
// module.
func (a *API) RegisterQueryable(
	queryable storage.SampleAndChunkQueryable,
	distributor Distributor,
) {
	// these routes are always registered to the default server
	a.RegisterRoute("/api/v1/user_stats", http.HandlerFunc(distributor.UserStatsHandler), true, "GET")

	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/user_stats"), http.HandlerFunc(distributor.UserStatsHandler), true, "GET")
}

// RegisterQueryAPI registers the Prometheus API routes with the provided handler.
func (a *API) RegisterQueryAPI(handler http.Handler) {
	a.RegisterRoute(path.Join(a.cfg.PrometheusHTTPPrefix, "/api/v1/read"), handler, true, "POST")
	a.RegisterRoute(path.Join(a.cfg.PrometheusHTTPPrefix, "/api/v1/query"), handler, true, "GET", "POST")
	a.RegisterRoute(path.Join(a.cfg.PrometheusHTTPPrefix, "/api/v1/query_range"), handler, true, "GET", "POST")
	a.RegisterRoute(path.Join(a.cfg.PrometheusHTTPPrefix, "/api/v1/query_exemplars"), handler, true, "GET", "POST")
	a.RegisterRoute(path.Join(a.cfg.PrometheusHTTPPrefix, "/api/v1/labels"), handler, true, "GET", "POST")
	a.RegisterRoute(path.Join(a.cfg.PrometheusHTTPPrefix, "/api/v1/label/{name}/values"), handler, true, "GET")
	a.RegisterRoute(path.Join(a.cfg.PrometheusHTTPPrefix, "/api/v1/series"), handler, true, "GET", "POST", "DELETE")
	a.RegisterRoute(path.Join(a.cfg.PrometheusHTTPPrefix, "/api/v1/metadata"), handler, true, "GET")

	// Register Legacy Routers
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/api/v1/read"), handler, true, "POST")
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/api/v1/query"), handler, true, "GET", "POST")
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/api/v1/query_range"), handler, true, "GET", "POST")
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/api/v1/query_exemplars"), handler, true, "GET", "POST")
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/api/v1/labels"), handler, true, "GET", "POST")
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/api/v1/label/{name}/values"), handler, true, "GET")
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/api/v1/series"), handler, true, "GET", "POST", "DELETE")
	a.RegisterRoute(path.Join(a.cfg.LegacyHTTPPrefix, "/api/v1/metadata"), handler, true, "GET")
}

// RegisterQueryFrontend registers the Prometheus routes supported by the
// Cortex querier service. Currently this can not be registered simultaneously
// with the Querier.
func (a *API) RegisterQueryFrontendHandler(h http.Handler) {
	a.RegisterQueryAPI(h)
}

func (a *API) RegisterQueryFrontend1(f *frontendv1.Frontend) {
	frontendv1pb.RegisterFrontendServer(a.server.GRPC, f)
}

func (a *API) RegisterQueryFrontend2(f *frontendv2.Frontend) {
	frontendv2pb.RegisterFrontendForQuerierServer(a.server.GRPC, f)
}

func (a *API) RegisterQueryScheduler(f *scheduler.Scheduler) {
	schedulerpb.RegisterSchedulerForFrontendServer(a.server.GRPC, f)
	schedulerpb.RegisterSchedulerForQuerierServer(a.server.GRPC, f)
}

// RegisterServiceMapHandler registers the Cortex structs service handler
// TODO: Refactor this code to be accomplished using the services.ServiceManager
// or a future module manager #2291
func (a *API) RegisterServiceMapHandler(handler http.Handler) {
	a.indexPage.AddLink(SectionAdminEndpoints, "/services", "Service Status")
	a.RegisterRoute("/services", handler, false, "GET")
}

func (a *API) RegisterMemberlistKV(handler http.Handler) {
	a.indexPage.AddLink(SectionAdminEndpoints, "/memberlist", "Memberlist Status")
	a.RegisterRoute("/memberlist", handler, false, "GET")
}
