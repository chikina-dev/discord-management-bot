package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	backups "github.com/chikina-dev/discord-management-bot/internal/backup"
	"github.com/chikina-dev/discord-management-bot/internal/cloud"
	"github.com/chikina-dev/discord-management-bot/internal/telemetry"
)

func backup() error {
	lifecycle, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(lifecycle, 45*time.Minute)
	defer cancel()
	meter := telemetry.New(os.Stdout)
	start := time.Now()
	client, err := cloud.New(ctx, region)
	if err != nil {
		return err
	}
	key, err := backups.Run(ctx, databasePath, backupBucket, client.S3)
	if err != nil {
		meter.Emit("backup_failed", map[string]any{"error": err.Error()}, map[string]float64{"BackupFailure": 1})
		return fmt.Errorf("backup failed: %w", err)
	}
	meter.Emit("backup_finished", map[string]any{"object_key": key}, map[string]float64{"BackupSuccess": 1, "BackupMilliseconds": float64(time.Since(start).Milliseconds())})
	return nil
}
