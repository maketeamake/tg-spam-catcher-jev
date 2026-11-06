package profile

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-pkgz/testutils/containers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/storage/engine"
)

// TestStore runs every query of the store on sqlite and on postgres, the two engines have different sql dialects
func TestStore(t *testing.T) {
	ctx := context.Background()
	engines := map[string]func(t *testing.T) *engine.SQL{
		"sqlite": func(t *testing.T) *engine.SQL {
			db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "profile.db"), "gid")
			require.NoError(t, err)
			return db
		},
		"postgres": func(t *testing.T) *engine.SQL {
			if testing.Short() {
				t.Skip("postgres needs docker")
			}
			pg := containers.NewPostgresTestContainerWithDB(ctx, t, "profile_test")
			t.Cleanup(func() { _ = pg.Close(ctx) })
			db, err := engine.NewPostgres(ctx, pg.ConnectionString(), "gid")
			require.NoError(t, err)
			return db
		},
	}
	for name, open := range engines {
		t.Run(name, func(t *testing.T) {
			db := open(t)
			t.Cleanup(func() { _ = db.Close() })
			s, err := NewStore(ctx, db)
			require.NoError(t, err)

			d := &Dossier{User: User{ID: 7, FirstName: "Eva"}, Bio: "bio", FetchedAt: time.Now()}
			require.NoError(t, s.SaveDossier(ctx, d))
			require.NoError(t, s.SaveDossier(ctx, d), "the second save updates the row")

			add := func(action Action) int64 {
				id, aerr := s.AddDecision(ctx, Record{UserID: 7, ChatID: -100, EventType: "reaction", Action: string(action),
					Reason: "r", TokensUsed: 10, LatencyMS: 5})
				require.NoError(t, aerr)
				return id
			}
			banID, first, second := add(ActionBan), add(ActionReview), add(ActionReview)
			assert.Less(t, first, second)

			rec, err := s.Decision(ctx, first)
			require.NoError(t, err)
			assert.Equal(t, string(ActionReview), rec.Action)

			since := time.Now().Add(-time.Hour)
			open, err := s.HasOpenReview(ctx, 7, second, since)
			require.NoError(t, err)
			assert.True(t, open, "the first review is open for the second")

			ok, err := s.AddReview(ctx, first, 1, VerdictSpam)
			require.NoError(t, err)
			assert.True(t, ok)
			ok, err = s.AddReview(ctx, first, 2, VerdictHam)
			require.NoError(t, err)
			assert.False(t, ok, "the first answer wins")
			require.NoError(t, s.RemoveReview(ctx, first))
			ok, err = s.AddReview(ctx, first, 2, VerdictHam)
			require.NoError(t, err)
			assert.True(t, ok, "a removed answer can be given again")
			cleared, err := s.AdminCleared(ctx, 7, since)
			require.NoError(t, err)
			assert.True(t, cleared)

			_, err = s.AddReview(ctx, banID, 1, VerdictHam)
			require.NoError(t, err)
			require.NoError(t, s.AbandonReviews(ctx, 7, first, " (card not sent)"))
			rec, err = s.Decision(ctx, first)
			require.NoError(t, err)
			assert.Equal(t, string(ActionReview), rec.Action, "an answered review stays")
			rec, err = s.Decision(ctx, second)
			require.NoError(t, err)
			assert.Equal(t, string(ActionSkip), rec.Action)
			assert.Equal(t, "r (card not sent)", rec.Reason)

			st, err := s.Stats(ctx, since, time.Hour)
			require.NoError(t, err)
			assert.Equal(t, Stats{Total: 3, Ban: 1, Review: 1, Skip: 1, Tokens: 30, Unbans: 1,
				LatencyP50: 5 * time.Millisecond, LatencyP95: 5 * time.Millisecond}, st)

			var buf bytes.Buffer
			require.NoError(t, s.ExportCSV(ctx, &buf, time.Hour))
			assert.Contains(t, buf.String(), "card not sent")
		})
	}
}
