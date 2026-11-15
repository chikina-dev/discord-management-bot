package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/chikina-dev/discord-management-bot/internal/app"
	"github.com/chikina-dev/discord-management-bot/internal/cloud"
	"github.com/chikina-dev/discord-management-bot/internal/gateway"
	"github.com/chikina-dev/discord-management-bot/internal/maintenance"
	"github.com/chikina-dev/discord-management-bot/internal/protocol"
	"github.com/chikina-dev/discord-management-bot/internal/store"
	"github.com/chikina-dev/discord-management-bot/internal/telemetry"
	"golang.org/x/crypto/acme/autocert"
)

const (
	region         = "ap-northeast-1"
	stateDir       = "/var/lib/discord-gateway"
	databasePath   = stateDir + "/gateway.db"
	publicHostname = "discord-gateway.chigayuki.com"
	publicURL      = "https://" + publicHostname
	adminAddress   = "10.203.0.1:8080"
	backupBucket   = "discord-gateway-backup-435684131663-ap-northeast-1"
	dnsZoneID      = "Z0673595TLUIY67DPP3F"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	command := "serve"
	if len(args) > 1 {
		return fmt.Errorf("usage: gateway [serve|backup|dns-update]")
	}
	if len(args) == 1 {
		command = args[0]
	}
	if command != "serve" && command != "backup" && command != "dns-update" {
		return fmt.Errorf("unknown command %q", command)
	}
	switch command {
	case "serve":
		return serve()
	case "backup":
		return backup()
	default:
		return dnsUpdate()
	}
}
func serve() error {
	lease, err := maintenance.AcquireShared(stateDir)
	if err != nil {
		return fmt.Errorf("cannot start while database maintenance is active: %w", err)
	}
	defer lease.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	meter := telemetry.New(os.Stdout)
	awsClients, err := cloud.New(ctx, region)
	if err != nil {
		return err
	}
	seed, err := awsClients.Get(ctx, "signing-seed")
	if err != nil {
		return fmt.Errorf("cannot load signing key: %w", err)
	}
	key, err := protocol.DecodePrivateKey(seed)
	if err != nil {
		return err
	}
	token, err := awsClients.Get(ctx, "bot-token")
	if err != nil {
		return fmt.Errorf("cannot load receiver Bot Token: %w", err)
	}
	guildID, err := awsClients.Get(ctx, "guild-id")
	if err != nil {
		return fmt.Errorf("cannot load target guild ID: %w", err)
	}
	s, err := store.Open(databasePath)
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err = s.Metadata(ctx, "backup_monitoring_started_at"); err == store.ErrNotFound {
		if err = s.SetMetadata(ctx, "backup_monitoring_started_at", protocol.Time(time.Now())); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	g := gateway.New(gateway.Config{Token: token, Intents: gateway.DefaultIntents, Store: s, Meter: meter})
	service, err := app.New(s, app.Config{GuildID: guildID, GuildName: "times_ちきな(裏)", BotToken: token, Key: key, ResultURL: publicURL + "/results", Meter: meter, Status: func() any { return g.Status() }, Reconnect: g.Reconnect})
	if err != nil {
		return err
	}
	g.SetOnDispatch(service.WakeEvents)
	management := &http.Server{Addr: adminAddress, Handler: service.ManagementHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	results := &http.Server{Addr: ":443", Handler: service.PublicHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	servers := []*http.Server{management, results}
	errs := make(chan error, 3)
	go func() { errs <- management.ListenAndServe() }()
	manager := &autocert.Manager{Prompt: autocert.AcceptTOS, Cache: autocert.DirCache(filepath.Join(stateDir, "acme")), HostPolicy: autocert.HostWhitelist(publicHostname)}
	results.TLSConfig = manager.TLSConfig()
	challenge := &http.Server{Addr: ":80", Handler: manager.HTTPHandler(http.NotFoundHandler()), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second}
	servers = append(servers, challenge)
	go func() { errs <- challenge.ListenAndServe() }()
	go func() { errs <- results.ListenAndServeTLS("", "") }()
	var loops sync.WaitGroup
	loops.Add(2)
	go func() { defer loops.Done(); g.Run(ctx) }()
	go func() { defer loops.Done(); service.Run(ctx) }()
	meter.Emit("service_started", map[string]any{"guild_id": guildID}, nil)
	select {
	case <-ctx.Done():
	case err = <-errs:
		cancel()
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for _, server := range servers {
		_ = server.Shutdown(shutdown)
	}
	loops.Wait()
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
func dnsUpdate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := cloud.New(ctx, region)
	if err != nil {
		return err
	}
	ip, err := cloud.PublicIPv4(ctx)
	if err != nil {
		return err
	}
	return c.UpdateDNS(ctx, dnsZoneID, publicHostname, ip)
}
