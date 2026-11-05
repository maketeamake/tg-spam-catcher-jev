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
	criteria, err := profile.LoadCriteria(settings.Profile.CriteriaFile)
	if err != nil {
		return nil, fmt.Errorf("can't load profile criteria: %w", err)
	}
	cfg := profile.Config{
		CacheTTL: settings.Profile.CacheTTL, CASNegativeTTL: profile.DefaultCASNegativeTTL,
		PhotosLimit: settings.Profile.PhotosLimit, PersonalPostsLimit: settings.Profile.PersonalPostsLimit,
		CheckJoin: settings.Profile.CheckJoin, ReviewTimeout: settings.Profile.ReviewTimeout, Criteria: criteria,
		Jev: profile.JevConfig{Token: settings.Jev.Token, APIBase: settings.Jev.APIBase, Model: settings.Profile.Model,
			Timeout: settings.Profile.Timeout},
		Thresholds: profile.Thresholds{CommentBan: settings.Profile.CommentBan, CommentReview: settings.Profile.CommentReview,
			ProfileBan: settings.Profile.ProfileBan, ProfileReview: settings.Profile.ProfileReview, Bait: settings.Profile.Bait,
			Photo: settings.Profile.Photo},
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
	f, err := os.OpenFile(settings.Profile.ExportCSV, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // operator-provided path
	if err != nil {
		return fmt.Errorf("can't create %s: %w", settings.Profile.ExportCSV, err)
	}
	defer f.Close() //nolint:errcheck // the export error is returned below
	if err = store.ExportCSV(ctx, f, settings.Profile.ReviewTimeout); err != nil {
		return fmt.Errorf("can't export decisions: %w", err)
	}
	return f.Close()
}
