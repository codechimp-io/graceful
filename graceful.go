package graceful

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloudflare/tableflip"
	"github.com/codechimp-io/log"
	"github.com/oklog/run"
)

var ShutdownTimeout = 3 * time.Second

// Run graceful http server
func Run(server *http.Server, pidfile string) {
	// configure graceful restart
	upg, err := tableflip.New(tableflip.Options{
		PIDFile: pidfile,
	})
	if err != nil {
		log.Errorf("Creating graceful upgrader failed: %v", err)
	}
	defer upg.Stop()

	// Do an upgrade on SIGHUP
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGHUP)
		for range sig {
			log.Info("Received SIGHUP, restaring gracefully...")
			err := upg.Upgrade()
			if err != nil {
				log.Errorf("Upgrade failed: %v", err)
			}
		}
	}()

	var group run.Group

	// Set up http server
	{
		ln, err := upg.Fds.Listen("tcp", server.Addr)
		if err != nil {
			log.Fatalf("Error creating new listener: %s", err)
		}

		group.Add(
			func() error {
				log.Infof("Listening on [%s] with pid [%d]", server.Addr, os.Getpid())

				return server.Serve(ln)
			},
			func(e error) {
				if e != nil {
					log.Fatalf("Failed to start HTTP Server: %v", e)
				}

				log.Info("Shutting HTTP Server down")

				ctx := context.Background()
				if ShutdownTimeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, ShutdownTimeout)
					defer cancel()
				}

				err := server.Shutdown(ctx)
				if err != nil {
					log.Errorf("Error shutting down HTTP server: %s", err)
				}

				_ = server.Close()
			},
		)
	}

	// Setup signal handler
	{
		var (
			cancelInterrupt = make(chan struct{})
			ch              = make(chan os.Signal, 2)
		)
		defer close(ch)

		group.Add(
			func() error {
				signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

				select {
				case sig := <-ch:
					switch sig {
					case syscall.SIGINT:
						log.Info("Received SIGINT, exiting gracefully...")
					case syscall.SIGTERM:
						log.Info("Received SIGTERM, exiting gracefully...")
					}

				case <-cancelInterrupt:
				}

				return nil
			},
			func(e error) {
				close(cancelInterrupt)
				signal.Stop(ch)
			},
		)
	}

	{
		group.Add(
			func() error {
				// Tell the parent we are ready
				_ = upg.Ready()

				// Wait for children to be ready
				// (or application shutdown)
				<-upg.Exit()

				return nil
			},
			func(e error) {
				upg.Stop()
			},
		)
	}

	err = group.Run()
	if err != nil {
		log.Fatalf("Error starting service: %s", err)
	}
}
