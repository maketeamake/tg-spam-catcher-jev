package main

import (
	"context"
	"fmt"
	"os"

	"github.com/umputun/tg-spam/app/config"
	"github.com/umputun/tg-spam/app/profile"
	"github.com/umputun/tg-spam/app/storage/engine"
	"github.com/umputun/tg-spam/lib/tgspam"
)

// makeProfileService builds the profile check from settings. CAS is used when it is configured.
func makeProfileService(ctx context.Context, settings *config.Settings, tg profile.TG, detector *tgspam.Detector,
	db *engine.SQL) (*profile.Service, error) {
	cfg, err := settings.ProfileConfig()
	if err != nil {
		return nil, fmt.Errorf("can't make profile config: %w", err)
	}
	store, err := profile.NewStore(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("can't make profile store: %w", err)
	}
	var cas profile.CASFunc
	if settings.CAS.API != "" && settings.CAS.Token != "" {
		cas = detector.CheckCAS
	}
	svc, err := profile.NewService(cfg, tg, store, cas, nil)
	if err != nil {
		return nil, fmt.Errorf("can't make profile service: %w", err)
	}
	return svc, nil
}

// exportProfileDecisions writes every profile decision with the admin verdicts to a csv file
func exportProfileDecisions(ctx context.Context, settings *config.Settings, db *engine.SQL) error {
	store, err := profile.NewStore(ctx, db)
	if err != nil {
		return fmt.Errorf("can't make profile store: %w", err)
	}
	path := settings.Profile.ExportCSV
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // operator-provided path
	if err != nil {
		return fmt.Errorf("can't create %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // the export error is returned below
	if err = store.ExportCSV(ctx, f, settings.Profile.ReviewTimeout); err != nil {
		return fmt.Errorf("can't export decisions: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("can't close %s: %w", path, err)
	}
	return nil
}
