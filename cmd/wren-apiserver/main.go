// Command wren-apiserver is the Wren control-plane API: it accepts run and
// project requests over HTTP/JSON and creates AgentRun custom resources in the
// cluster for the operator to reconcile (spec §5.2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/summiteight/wren/internal/apiserver"
	"github.com/summiteight/wren/internal/coreapi"
	"github.com/summiteight/wren/internal/launcher"
	"github.com/summiteight/wren/internal/store"
)

func main() {
	addr := flag.String("addr", ":8090", "HTTP listen address")
	storeKind := flag.String("store", "memory", "store backend: memory|postgres")
	flag.Parse()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Fatalf("load kube config: %v", err)
	}
	lc, err := launcher.NewK8s(cfg)
	if err != nil {
		log.Fatalf("build launcher: %v", err)
	}

	// Store selection: memory (default, dev/tests) or durable Postgres. The DSN
	// comes from DATABASE_URL (spec §5.2 / implementation-plan §WS-3).
	st, cleanup, err := buildStore(*storeKind)
	if err != nil {
		log.Fatalf("build store: %v", err)
	}
	defer cleanup()

	// `wren install` sets WREN_DEFAULT_RUN_NAMESPACE to its --run-namespace so a
	// project registered with no --namespace lands runs where install wrote the
	// credential Secrets (WS-15 Part A). Empty keeps the per-user-prefix fallback.
	defaults := coreapi.DefaultDefaults()
	if ns := os.Getenv("WREN_DEFAULT_RUN_NAMESPACE"); ns != "" {
		defaults.DefaultNamespace = ns
	}
	svc := coreapi.New(st, lc, defaults)

	// Reconcile-on-boot: re-learn in-flight runs from the AgentRun CRs so a
	// restarted apiserver (or one that just migrated stores) does not forget
	// them. Non-fatal: a fresh install with an unreachable list still serves.
	if n, err := svc.ReconcileFromCluster(context.Background()); err != nil {
		log.Printf("reconcile-on-boot: %v (continuing)", err)
	} else if n > 0 {
		log.Printf("reconcile-on-boot: re-learned %d run(s) from the cluster", n)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           apiserver.New(svc, lc).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Slowloris-style hardening (Go's http.Server doc recommends setting
		// these explicitly — the zero value is "no timeout"). ReadTimeout/
		// WriteTimeout are sized for the quick JSON request/response calls
		// that make up the rest of this API; they'd truncate the long-lived
		// `GET /v1/runs/{id}/logs?follow=true` stream (WS-4) if applied to
		// it too, so that handler explicitly disables its own write deadline
		// via http.ResponseController — see runLogs in internal/apiserver.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("wren-apiserver listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Print("wren-apiserver stopped")
}

// buildStore selects the store backend. It returns the Store and a cleanup func
// (a no-op for memory; pool.Close for postgres).
func buildStore(kind string) (store.Store, func(), error) {
	switch kind {
	case "", "memory":
		return store.NewMemory(), func() {}, nil
	case "postgres":
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			return nil, nil, errors.New("--store=postgres requires DATABASE_URL")
		}
		pg, err := store.NewPostgres(context.Background(), dsn)
		if err != nil {
			return nil, nil, err
		}
		return pg, pg.Close, nil
	default:
		return nil, nil, fmt.Errorf("unknown store %q (want memory|postgres)", kind)
	}
}
