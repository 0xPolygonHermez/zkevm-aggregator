package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"time"

	"github.com/0xPolygonHermez/zkevm-aggregator"
	"github.com/0xPolygonHermez/zkevm-aggregator/aggregator"
	"github.com/0xPolygonHermez/zkevm-aggregator/config"
	"github.com/0xPolygonHermez/zkevm-aggregator/db"
	"github.com/0xPolygonHermez/zkevm-aggregator/etherman"
	"github.com/0xPolygonHermez/zkevm-aggregator/event"
	"github.com/0xPolygonHermez/zkevm-aggregator/event/nileventstorage"
	"github.com/0xPolygonHermez/zkevm-aggregator/event/pgeventstorage"
	"github.com/0xPolygonHermez/zkevm-aggregator/log"
	"github.com/0xPolygonHermez/zkevm-aggregator/metrics"
	"github.com/0xPolygonHermez/zkevm-aggregator/state"
	"github.com/0xPolygonHermez/zkevm-aggregator/state/pgstatestorage"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v2"
)

func start(cliCtx *cli.Context) error {
	c, err := config.Load(cliCtx, true)
	if err != nil {
		return err
	}
	setupLog(c.Aggregator.Log)

	if c.Aggregator.Log.Environment == log.EnvironmentDevelopment {
		zkevm.PrintVersion(os.Stdout)
		log.Info("Starting application")
	} else if c.Aggregator.Log.Environment == log.EnvironmentProduction {
		logVersion()
	}

	if c.Metrics.Enabled {
		metrics.Init()
	}

	// Migrations
	if !cliCtx.Bool(config.FlagMigrations) {
		log.Infof("Running DB migrations host: %s:%s db:%s user:%s", c.Aggregator.DB.Host, c.Aggregator.DB.Port, c.Aggregator.DB.Name, c.Aggregator.DB.User)
		runAggregatorMigrations(c.Aggregator.DB)
	}

	checkAggregatorMigrations(c.Aggregator.DB)

	var (
		eventLog     *event.EventLog
		eventStorage event.Storage
		cancelFuncs  []context.CancelFunc
	)

	if c.EventLog.DB.Name != "" {
		eventStorage, err = pgeventstorage.NewPostgresEventStorage(c.EventLog.DB)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		eventStorage, err = nileventstorage.NewNilEventStorage()
		if err != nil {
			log.Fatal(err)
		}
	}
	eventLog = event.NewEventLog(c.EventLog, eventStorage)

	// Core State DB
	stateSqlDB, err := db.NewSQLDB(c.Aggregator.DB)
	if err != nil {
		log.Fatal(err)
	}

	etherman, err := newEtherman(*c)
	if err != nil {
		log.Fatal(err)
	}

	// READ CHAIN ID FROM POE SC
	l2ChainID, err := etherman.GetL2ChainID()
	if err != nil {
		log.Fatal(err)
	}

	st := newState(c, l2ChainID, stateSqlDB, eventLog)

	c.Aggregator.ChainID = l2ChainID

	ev := &event.Event{
		ReceivedAt: time.Now(),
		Source:     event.Source_Node,
		Level:      event.Level_Info,
		EventID:    event.EventID_NodeComponentStarted,
	}

	if c.Metrics.ProfilingEnabled {
		go startProfilingHttpServer(c.Metrics)
	}

	// Populate Network config
	c.Aggregator.Synchronizer.Etherman.Contracts.GlobalExitRootManagerAddr = c.NetworkConfig.L1Config.GlobalExitRootManagerAddr
	c.Aggregator.Synchronizer.Etherman.Contracts.RollupManagerAddr = c.NetworkConfig.L1Config.RollupManagerAddr
	c.Aggregator.Synchronizer.Etherman.Contracts.ZkEVMAddr = c.NetworkConfig.L1Config.ZkEVMAddr

	ev.Component = event.Component_Aggregator
	ev.Description = "Running aggregator"
	err = eventLog.LogEvent(cliCtx.Context, ev)
	if err != nil {
		log.Fatal(err)
	}
	go runAggregator(cliCtx.Context, c.Aggregator, etherman, st)

	if c.Metrics.Enabled {
		go startMetricsHttpServer(c.Metrics)
	}

	waitSignal(cancelFuncs)

	return nil
}

func setupLog(c log.Config) {
	log.Init(c)
}

func runAggregatorMigrations(c db.Config) {
	runMigrations(c, db.AggregatorMigrationName)
}

func checkAggregatorMigrations(c db.Config) {
	err := db.CheckMigrations(c, db.AggregatorMigrationName)
	if err != nil {
		log.Fatal(err)
	}
}

func runMigrations(c db.Config, name string) {
	log.Infof("running migrations for %v", name)
	err := db.RunMigrationsUp(c, name)
	if err != nil {
		log.Fatal(err)
	}
}

func newEtherman(c config.Config) (*etherman.Client, error) {
	config := etherman.Config{
		URL: c.Aggregator.EthTxManager.Etherman.URL,
	}
	return etherman.NewClient(config, c.NetworkConfig.L1Config)
}

func runAggregator(ctx context.Context, config aggregator.Config, etherman *etherman.Client, st *state.State) {
	agg, err := aggregator.New(ctx, config, st, etherman)
	if err != nil {
		log.Fatal(err)
	}
	err = agg.Start(ctx)
	if err != nil {
		log.Fatal(err)
	}
}

func waitSignal(cancelFuncs []context.CancelFunc) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	for sig := range signals {
		switch sig {
		case os.Interrupt, os.Kill:
			log.Info("terminating application gracefully...")

			exitStatus := 0
			for _, cancel := range cancelFuncs {
				cancel()
			}
			os.Exit(exitStatus)
		}
	}
}

func newState(c *config.Config, l2ChainID uint64, sqlDB *pgxpool.Pool, eventLog *event.EventLog) *state.State {
	stateCfg := state.Config{
		DB:      c.Aggregator.DB,
		ChainID: l2ChainID,
	}

	stateDb := pgstatestorage.NewPostgresStorage(stateCfg, sqlDB)

	st := state.NewState(stateCfg, stateDb, eventLog)
	return st
}

func startProfilingHttpServer(c metrics.Config) {
	const two = 2
	mux := http.NewServeMux()
	address := fmt.Sprintf("%s:%d", c.ProfilingHost, c.ProfilingPort)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Errorf("failed to create tcp listener for profiling: %v", err)
		return
	}
	mux.HandleFunc(metrics.ProfilingIndexEndpoint, pprof.Index)
	mux.HandleFunc(metrics.ProfileEndpoint, pprof.Profile)
	mux.HandleFunc(metrics.ProfilingCmdEndpoint, pprof.Cmdline)
	mux.HandleFunc(metrics.ProfilingSymbolEndpoint, pprof.Symbol)
	mux.HandleFunc(metrics.ProfilingTraceEndpoint, pprof.Trace)
	profilingServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: two * time.Minute,
		ReadTimeout:       two * time.Minute,
	}
	log.Infof("profiling server listening on port %d", c.ProfilingPort)
	if err := profilingServer.Serve(lis); err != nil {
		if err == http.ErrServerClosed {
			log.Warnf("http server for profiling stopped")
			return
		}
		log.Errorf("closed http connection for profiling server: %v", err)
		return
	}
}

func startMetricsHttpServer(c metrics.Config) {
	const ten = 10
	mux := http.NewServeMux()
	address := fmt.Sprintf("%s:%d", c.Host, c.Port)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Errorf("failed to create tcp listener for metrics: %v", err)
		return
	}
	mux.Handle(metrics.Endpoint, promhttp.Handler())

	metricsServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: ten * time.Second,
		ReadTimeout:       ten * time.Second,
	}
	log.Infof("metrics server listening on port %d", c.Port)
	if err := metricsServer.Serve(lis); err != nil {
		if err == http.ErrServerClosed {
			log.Warnf("http server for metrics stopped")
			return
		}
		log.Errorf("closed http connection for metrics server: %v", err)
		return
	}
}

func logVersion() {
	log.Infow("Starting application",
		// node version is already logged by default
		"gitRevision", zkevm.GitRev,
		"gitBranch", zkevm.GitBranch,
		"goVersion", runtime.Version(),
		"built", zkevm.BuildDate,
		"os/arch", fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	)
}
