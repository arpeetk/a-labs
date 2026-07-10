// Command wren-apiserver is the Wren control-plane API: it accepts run and
// project requests over HTTP/JSON and creates AgentRun custom resources in the
// cluster for the operator to reconcile (spec §5.2).
package main

import (
	"context"
	"errors"
	"flag"
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
	flag.Parse()

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Fatalf("load kube config: %v", err)
	}
	lc, err := launcher.NewK8s(cfg)
	if err != nil {
		log.Fatalf("build launcher: %v", err)
	}

	// M0 uses an in-memory store; the Postgres implementation is a fast-follow.
	svc := coreapi.New(store.NewMemory(), lc, coreapi.DefaultDefaults())
	srv := &http.Server{
		Addr:              *addr,
		Handler:           apiserver.New(svc).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
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
