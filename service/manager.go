package service

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Service represents any service that can be gracefully shutdown
type Service interface {
	Start()
	Shutdown(ctx context.Context) error
}

// Manager manages multiple services and handles graceful shutdown
type Manager struct {
	services []Service
	mu       sync.Mutex
}

// NewManager creates a new service manager
func NewManager() *Manager {
	return &Manager{}
}

// Register adds a service to the manager
func (m *Manager) Register(service Service) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.services = append(m.services, service)
}

// StartAll starts all registered services
func (m *Manager) StartAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, service := range m.services {
		service.Start()
	}
}

// ShutdownAll gracefully shuts down all services
func (m *Manager) ShutdownAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var wg sync.WaitGroup
	errors := make(chan error, len(m.services))

	for _, service := range m.services {
		wg.Add(1)
		go func(svc Service) {
			defer wg.Done()
			if err := svc.Shutdown(ctx); err != nil {
				errors <- err
			}
		}(service)
	}

	wg.Wait()
	close(errors)

	// Collect any errors
	var firstError error
	for err := range errors {
		if firstError == nil {
			firstError = err
		}
		slog.Error("service shutdown error", "error", err)
	}

	return firstError
}

// WaitForShutdown waits for shutdown signals and gracefully stops all services
func (m *Manager) WaitForShutdown(signals ...os.Signal) {
	if len(signals) < 1 {
		signals = append(signals, syscall.SIGTERM, syscall.SIGINT)
	}

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, signals...)

	<-ch
	signal.Stop(ch)
	close(ch)

	slog.Info("shutdown signal received, gracefully stopping services...")

	// Create a context with timeout for graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := m.ShutdownAll(ctx); err != nil {
		slog.Error("error during shutdown", "error", err)
		os.Exit(1)
	}

	slog.Info("all services stopped gracefully")
}