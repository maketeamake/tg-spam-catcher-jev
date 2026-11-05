//go:build live

package profile

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	tbapi "github.com/OvyFlash/telegram-bot-api"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/storage/engine"
)

// TestLiveJev runs synthetic cases against the real jev API: go test -tags live -run TestLiveJev -v ./app/profile
// The key is read from TYPESAFE_API_KEY and never printed. Add cases found in the dry-run CSV before every threshold change.
func TestLiveJev(t *testing.T) {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Skip("TYPESAFE_API_KEY is not set")
	}

	img := image.NewRGBA(image.Rect(0, 0, 640, 640))
	for y := 0; y < 640; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x / 3), G: uint8(y / 3), B: 140, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(buf.Bytes()) }))
	defer files.Close()

	const post = "We released version 2.0 of our open source Go library: faster parsing and a new plugin API."
	tests := []struct {
		name    string
		bio     string
		photo   bool
		kind    Kind
		comment string
		want    []Action // acceptable actions
	}{
		{"on-topic comment, empty profile", "", false, KindComment, "Does the plugin API support hot reload? We use it in a long running service.", []Action{ActionPass}},
		{"short friendly comment", "Go developer from Berlin", false, KindComment, "Great news, thanks for the release!", []Action{ActionPass}},
		{"job scam comment", "", false, KindComment, "Earn $500 a day from home, no experience needed! Write me in DM for details", []Action{ActionBan, ActionReview}},
		{"escort bio with template comment", "Young and lonely. Dating, private photos, write me in DM", true, KindComment, "Nice post, very interesting!", []Action{ActionBan, ActionReview}},
		{"escort bio reaction", "Young and lonely. Dating, private photos, write me in DM", true, KindReaction, "", []Action{ActionBan, ActionReview}},
		{"drug shop bio reaction", "Закладки, мефедрон, шишки. Работаем 24/7, заказ через бота", false, KindReaction, "", []Action{ActionBan, ActionReview}},
		{"plain bio reaction", "Backend engineer, coffee and hiking", true, KindReaction, "", []Action{ActionPass}},
		{"russian on-topic comment", "", false, KindComment, "А плагины можно подгружать без перезапуска сервиса?", []Action{ActionPass}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg := &fakeTG{bio: tt.bio, fileURL: files.URL}
			if tt.photo {
				tg.photoID = "p1"
			}
			db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "live.db"), "gid")
			require.NoError(t, err)
			defer db.Close()
			store, err := NewStore(context.Background(), db)
			require.NoError(t, err)
			cfg, err := DefaultConfig()
			require.NoError(t, err)
			cfg.Jev.Token = key
			svc, err := NewService(cfg, tg, store, func(context.Context, int64) (bool, string, error) { return false, "", nil }, nil)
			require.NoError(t, err)

			ev := Event{Kind: tt.kind, User: User{ID: 42, FirstName: "Test"}, ChatID: -1001, MessageID: 1, PostText: post, Comment: tt.comment}
			res := svc.Check(context.Background(), ev)
			t.Logf("%s: %s (%s) %s tokens=%d latency=%v", tt.name, res.Action, res.Reason, res.Scores, res.Tokens, res.Latency)
			require.NoError(t, res.Err, "a failed call is a failed smoke test, including image acceptance")
			assert.Contains(t, tt.want, res.Action, "%s %s", res.Reason, res.Scores)
		})
	}
}

// TestLiveTelegramDossier reads a real profile through the bot API and judges it with the real jev:
// TELEGRAM_TOKEN=... TYPESAFE_API_KEY=... LIVE_USER_ID=... go test -tags live -run TestLiveTelegramDossier -v ./app/profile
// The user must share a chat with the bot. Nothing is changed in Telegram.
func TestLiveTelegramDossier(t *testing.T) {
	key, token, uid := os.Getenv("TYPESAFE_API_KEY"), os.Getenv("TELEGRAM_TOKEN"), os.Getenv("LIVE_USER_ID")
	if key == "" || token == "" || uid == "" {
		t.Skip("TYPESAFE_API_KEY, TELEGRAM_TOKEN and LIVE_USER_ID must be set")
	}
	id, err := strconv.ParseInt(uid, 10, 64)
	require.NoError(t, err)
	api, err := tbapi.NewBotAPI(token)
	require.NoError(t, err)

	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "live.db"), "gid")
	require.NoError(t, err)
	defer db.Close()
	store, err := NewStore(context.Background(), db)
	require.NoError(t, err)
	cfg, err := DefaultConfig()
	require.NoError(t, err)
	cfg.Jev.Token = key
	svc, err := NewService(cfg, api, store, nil, nil)
	require.NoError(t, err)

	res := svc.Check(context.Background(), Event{Kind: KindReaction, User: User{ID: id}, ChatID: -1})
	require.NoError(t, res.Err)
	require.NotNil(t, res.Dossier)
	d := res.Dossier
	t.Logf("bio=%d chars, photos=%d, channel=%v, action=%s (%s) %s tokens=%d", len(d.Bio), len(d.Photos), d.Channel != nil,
		res.Action, res.Reason, res.Scores, res.Tokens)
}
