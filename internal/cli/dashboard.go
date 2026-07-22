package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/nn1a/kanban/internal/dashboard"
)

func (a *App) runDashboard(ctx context.Context, opts options) error {
	dbPath, err := a.dispatcherDBPath(opts.value("db"))
	if err != nil {
		return err
	}
	cliPath, err := os.Executable()
	if err != nil {
		return err
	}
	port, err := numberOption(opts.value("port"), 8420)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("invalid dashboard port: %s", opts.value("port"))
	}
	host := opts.value("host")
	if host == "" {
		host = "127.0.0.1"
	}
	server, err := dashboard.Start(ctx, dashboard.Options{
		DBPath: dbPath, CLIPath: cliPath, Host: host, Port: port, Token: opts.value("token"),
		OnLog: func(message string) { _, _ = fmt.Fprintf(a.Stderr, "[taskcircuit] %s\n", message) },
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(a.Stdout, "%s/?token=%s\n", server.URL, url.QueryEscape(server.Token)); err != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Close(shutdown)
		return err
	}
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Close(shutdown)
}
