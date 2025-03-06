package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/librescoot/pm-service/internal/config"
	"github.com/librescoot/pm-service/internal/service"
)

var version = "0.1.0" // Default version, can be overridden during build

func main() {
	var logger *log.Logger
	if os.Getenv("INVOCATION_ID") != "" {
		logger = log.New(os.Stdout, "", 0)
	} else {
		logger = log.New(os.Stdout, "librescoot-pm: ", log.LstdFlags|log.Lmsgprefix)
	}

	cfg := config.New()
	cfg.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc, err := service.New(cfg, logger)
	if err != nil {
		log.Fatalf("Failed to create service: %v", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		logger.Println("Received termination signal")
		cancel()
	}()

	logger.Printf("Starting power management service v%s", version)
	if err := svc.Run(ctx); err != nil {
		log.Fatalf("Service failed: %v", err)
	}
}
