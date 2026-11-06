package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/storage/engine"
)

// fakeTG serves canned profiles and counts calls
type fakeTG struct {
	mu        sync.Mutex
	bio       string
	photoID   string
	fileURL   string
	chatCalls int
	chatErr   error
}

func (f *fakeTG) GetChat(tbapi.ChatInfoConfig) (tbapi.ChatFullInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatCalls++
	return tbapi.ChatFullInfo{Bio: f.bio}, f.chatErr
}

func (f *fakeTG) GetUserProfilePhotos(tbapi.UserProfilePhotosConfig) (tbapi.UserProfilePhotos, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.photoID == "" {
		return tbapi.UserProfilePhotos{}, nil
	}
	return tbapi.UserProfilePhotos{TotalCount: 1, Photos: [][]tbapi.PhotoSize{{
		{FileID: "small", FileUniqueID: "u-small-" + f.photoID, Width: 160, Height: 160},
		{FileID: "big", FileUniqueID: "u-big-" + f.photoID, Width: 640, Height: 640},
	}}}, nil
}

func (f *fakeTG) GetFileDirectURL(string) (string, error) { return f.fileURL, nil }

func (f *fakeTG) GetUserPersonalChatMessages(tbapi.UserPersonalChatMessagesConfig) ([]tbapi.Message, error) {
	return nil, errors.New("no personal chat")
}

// fakeJev answers every asked question from a script and records requests
type fakeJev struct {
	calls     atomic.Int32
	status    atomic.Int32
	images    atomic.Int32
	imgHeader atomic.Bool
	noul      map[string]float64 // answers for noul questions, default 0.01
	class     map[string]float64
	score     float64
}

func (f *fakeJev) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
		f.imgHeader.Store(r.Header.Get("TypeSafe-Danger-Zone-Images") == "true")
		var raw []byte
		nimg := 0
		mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(p)
				if p.FormName() == "request" {
					raw = data
				} else {
					f.images.Add(1)
					nimg++
				}
			}
		} else {
			raw, _ = io.ReadAll(r.Body)
		}
		if st := int(f.status.Load()); st != 0 && (st != http.StatusBadRequest || nimg != 0) {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
			return
		}
		var req struct {
			Model     string
			Questions map[string]struct{ Type string }
		}
		if !assert.NoError(t, json.Unmarshal(raw, &req)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, DefaultJevModel, req.Model)
		answers := map[string]any{}
		for name, q := range req.Questions {
			switch q.Type {
			case "noul":
				p, ok := f.noul[name]
				if !ok {
					p = 0.01
				}
				answers[name] = map[string]any{"type": "noul", "noul": p}
			case "choice":
				probs := map[string]float64{"ok": 1}
				for c, p := range f.class {
					probs[c], probs["ok"] = p, probs["ok"]-p
				}
				answers[name] = map[string]any{"type": "choice", "choice": "ok", "probabilities": probs}
			case "score":
				answers[name] = map[string]any{"type": "score", "score": f.score}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": DefaultJevModel, "answers": answers,
			"usage": map[string]int{"input_tokens": 100, "output_tokens": 10}})
	})
}

type env struct {
	svc  *Service
	tg   *fakeTG
	jev  *fakeJev
	cas  atomic.Int32
	casF func(int64) bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{jev: &fakeJev{}, casF: func(int64) bool { return false }}
	jevSrv := httptest.NewServer(e.jev.handler(t))
	t.Cleanup(jevSrv.Close)

	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 640, 640)), nil))
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(buf.Bytes()) }))
	t.Cleanup(fileSrv.Close)
	e.tg = &fakeTG{fileURL: fileSrv.URL + "/photo.jpg"}

	db, err := engine.NewSqlite(filepath.Join(t.TempDir(), "test.db"), "gid")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewStore(context.Background(), db)
	require.NoError(t, err)

	cfg, err := DefaultConfig()
	require.NoError(t, err)
	cfg.Jev.Token, cfg.Jev.APIBase = "test-key", jevSrv.URL
	cfg.Thresholds = DefaultThresholds()
	cas := func(_ context.Context, id int64) (bool, string, error) {
		e.cas.Add(1)
		return e.casF(id), "listed", nil
	}
	e.svc, err = NewService(cfg, e.tg, store, cas, nil)
	require.NoError(t, err)
	return e
}

var user = User{ID: 42, FirstName: "Alina", Username: "alina_xx"}

func TestService_CASHitSkipsJev(t *testing.T) {
	e := newEnv(t)
	e.casF = func(int64) bool { return true }
	res := e.svc.Check(context.Background(), Event{Kind: KindReaction, User: user, ChatID: -100})
	assert.Equal(t, ActionBan, res.Action)
	assert.True(t, res.CASHit)
	assert.Zero(t, e.jev.calls.Load(), "jev must not be called on a CAS hit")
	assert.NotZero(t, res.DecisionID)

	e.svc.Check(context.Background(), Event{Kind: KindReaction, User: user, ChatID: -100})
	assert.EqualValues(t, 1, e.cas.Load(), "a CAS hit is cached")
}

func TestService_EscortProfileAndBaitComment(t *testing.T) {
	e := newEnv(t)
	e.tg.bio, e.tg.photoID = "Скучаю 💋 пиши", "a"
	e.jev.noul = map[string]float64{"bait": 0.9, "profile_adult": 0.97, "photo_provocative": 0.95}
	res := e.svc.Check(context.Background(), Event{Kind: KindComment, User: user, ChatID: -100, MessageID: 7,
		PostText: "Новый закон о налогах", Comment: "Ого, как интересно!"})
	assert.Equal(t, ActionBan, res.Action, res.Reason)
	assert.True(t, e.jev.imgHeader.Load(), "images need the danger zone header")
	assert.EqualValues(t, 1, e.jev.images.Load(), "the avatar goes as a multipart file")
	assert.Equal(t, 110, res.Tokens)
	assert.EqualValues(t, 1, e.jev.calls.Load(), "all questions in one request")
}

func TestService_CleanAccountPasses(t *testing.T) {
	e := newEnv(t)
	e.jev.score = 2
	res := e.svc.Check(context.Background(), Event{Kind: KindComment, User: user, ChatID: -100, PostText: "Пост", Comment: "Согласен с пунктом про НДС"})
	assert.Equal(t, ActionPass, res.Action, res.Reason)
	assert.Zero(t, e.jev.images.Load())
}

func TestService_JevFailureNeverSanctions(t *testing.T) {
	e := newEnv(t)
	e.tg.bio = "что угодно"
	e.jev.status.Store(http.StatusInternalServerError)
	res := e.svc.Check(context.Background(), Event{Kind: KindComment, User: user, ChatID: -100, Comment: "Привет"})
	assert.Equal(t, ActionSkip, res.Action)
	require.Error(t, res.Err)
	assert.EqualValues(t, 2, e.jev.calls.Load(), "one retry on a server error")
}

func TestService_ImageRejectionFallsBackToText(t *testing.T) {
	e := newEnv(t)
	e.tg.bio, e.tg.photoID = "bio", "a"
	e.jev.status.Store(http.StatusBadRequest)
	res := e.svc.Check(context.Background(), Event{Kind: KindReaction, User: user, ChatID: -100})
	assert.Equal(t, ActionPass, res.Action, res.Reason)
	assert.EqualValues(t, 2, e.jev.calls.Load(), "second request has no images")
}

func TestService_ProfileVerdictAndDossierCache(t *testing.T) {
	e := newEnv(t)
	e.tg.bio, e.tg.photoID = "bio", "a"
	ev := Event{Kind: KindReaction, User: user, ChatID: -100}
	e.svc.Check(context.Background(), ev)
	e.svc.Check(context.Background(), ev)
	assert.EqualValues(t, 1, e.jev.calls.Load(), "profile answers are reused for reactions")
	assert.Equal(t, 2, e.tg.chatCalls, "dossier is cached, only the freshness check reads the chat")

	e.tg.mu.Lock()
	e.tg.photoID = "b" // the user changed the avatar
	e.tg.mu.Unlock()
	e.svc.Check(context.Background(), ev)
	assert.EqualValues(t, 2, e.jev.calls.Load(), "new avatar resets the caches")

	e.tg.mu.Lock()
	e.tg.bio = "escort, write me" // the user changed the bio
	e.tg.mu.Unlock()
	e.svc.Check(context.Background(), ev)
	assert.EqualValues(t, 3, e.jev.calls.Load(), "new bio resets the caches")
}

func TestService_EmptyProfileAndJoinOff(t *testing.T) {
	e := newEnv(t)
	res := e.svc.Check(context.Background(), Event{Kind: KindReaction, User: user, ChatID: -100})
	assert.Equal(t, ActionPass, res.Action)
	res = e.svc.Check(context.Background(), Event{Kind: KindJoin, User: user, ChatID: -100})
	assert.Equal(t, ActionPass, res.Action)
	assert.Zero(t, e.jev.calls.Load())
}

func TestService_StoreDownIsNoSanction(t *testing.T) {
	e := newEnv(t)
	e.casF = func(int64) bool { return true }
	require.NoError(t, e.svc.store.Close())
	res := e.svc.Check(context.Background(), Event{Kind: KindReaction, User: user, ChatID: -100})
	assert.Equal(t, ActionSkip, res.Action)
	require.Error(t, res.Err)
}

func TestService_ReviewFlowAndStats(t *testing.T) {
	e := newEnv(t)
	e.tg.bio = "bio"
	e.jev.noul = map[string]float64{"profile_scam": 0.7}
	ctx := context.Background()
	res := e.svc.Check(ctx, Event{Kind: KindReaction, User: user, ChatID: -100})
	require.Equal(t, ActionReview, res.Action, res.Reason)
	assert.False(t, e.svc.HasOpenReview(ctx, user.ID, res.DecisionID), "the decision itself does not count")
	assert.True(t, e.svc.HasOpenReview(ctx, user.ID, res.DecisionID+1), "a later decision sees it, the first one does not see the later")

	e.svc.cfg.ReviewTimeout = time.Nanosecond
	assert.False(t, e.svc.HasOpenReview(ctx, user.ID, res.DecisionID+1), "an expired review does not hide new cards")
	e.svc.cfg.ReviewTimeout = time.Hour

	_, first, err := e.svc.Resolve(ctx, res.DecisionID, 1, VerdictHam)
	require.NoError(t, err)
	assert.True(t, first)
	_, first, err = e.svc.Resolve(ctx, res.DecisionID, 2, VerdictSpam)
	require.NoError(t, err)
	assert.False(t, first, "the second answer is ignored")
	assert.False(t, e.svc.HasOpenReview(ctx, user.ID, res.DecisionID+1), "answered")
	assert.True(t, e.svc.Cleared(ctx, user.ID, time.Now().Add(-time.Minute)))

	require.NoError(t, e.svc.Unresolve(ctx, res.DecisionID), "a failed action takes the verdict back")
	_, first, err = e.svc.Resolve(ctx, res.DecisionID, 2, VerdictSpam)
	require.NoError(t, err)
	assert.True(t, first, "the next press acts again")

	st, err := e.svc.store.Stats(ctx, time.Now().Add(-time.Hour), time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, st.Review)
	assert.Equal(t, 1, st.Total)

	var csv bytes.Buffer
	require.NoError(t, e.svc.store.ExportCSV(ctx, &csv, time.Hour))
	assert.Contains(t, csv.String(), "reviewed")
	assert.Contains(t, csv.String(), "reaction")
}

func TestService_AbandonedReviewDoesNotHideCards(t *testing.T) {
	e := newEnv(t)
	e.tg.bio = "bio"
	e.jev.noul = map[string]float64{"profile_scam": 0.7}
	ctx := context.Background()
	first := e.svc.Check(ctx, Event{Kind: KindReaction, User: user, ChatID: -100})
	require.Equal(t, ActionReview, first.Action, first.Reason)
	second := e.svc.Check(ctx, Event{Kind: KindReaction, User: user, ChatID: -100}) // got no card, hidden by the first
	require.Equal(t, ActionReview, second.Action, second.Reason)

	require.NoError(t, e.svc.Abandon(ctx, user.ID, first.DecisionID, "card not sent"))
	assert.False(t, e.svc.HasOpenReview(ctx, user.ID, second.DecisionID+1), "both reviews are given up")
	rec, err := e.svc.store.Decision(ctx, second.DecisionID)
	require.NoError(t, err)
	assert.Equal(t, string(ActionSkip), rec.Action)
	assert.Contains(t, rec.Reason, "(card not sent)")
}

func TestService_UnreadProfileIsSkippedAndReadAgain(t *testing.T) {
	e := newEnv(t)
	e.tg.chatErr = errors.New("timeout")
	ev := Event{Kind: KindReaction, User: user, ChatID: -100}
	res := e.svc.Check(context.Background(), ev)
	assert.Equal(t, ActionSkip, res.Action, "an unread profile is not an empty one")

	e.tg.mu.Lock()
	e.tg.chatErr, e.tg.bio = nil, "закладки 24/7"
	e.tg.mu.Unlock()
	e.jev.noul = map[string]float64{"profile_drugs": 0.99}
	res = e.svc.Check(context.Background(), ev)
	assert.Equal(t, ActionBan, res.Action, "the partial dossier was not cached: %s", res.Reason)
}

func TestService_ReactionSkippedWhileUserIsChecked(t *testing.T) {
	e := newEnv(t)
	e.tg.bio = "bio"
	unlock := e.svc.locks.lock(user.ID)
	res := e.svc.Check(context.Background(), Event{Kind: KindReaction, User: user, ChatID: -100})
	unlock()
	assert.Equal(t, ActionSkip, res.Action)
	assert.Zero(t, e.jev.calls.Load())
	assert.Empty(t, e.svc.locks.m)
}

func TestService_NoPhotoFilesOnDisk(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	e.tg.bio, e.tg.photoID = "bio", "a"
	e.svc.Check(context.Background(), Event{Kind: KindReaction, User: user, ChatID: -100})
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "avatars live in memory only")
}

func TestPickSize(t *testing.T) {
	sizes := []tbapi.PhotoSize{{Width: 160, Height: 160, FileID: "a"}, {Width: 320, Height: 320, FileID: "b"},
		{Width: 640, Height: 640, FileID: "c"}, {Width: 2000, Height: 2000, FileID: "d"}}
	got, ok := pickSize(sizes)
	require.True(t, ok)
	assert.Equal(t, "c", got.FileID)
	_, ok = pickSize(sizes[:1])
	assert.False(t, ok, "a 160 px avatar is below the jev limit")
}

func TestKeyedLockSerialisesPerKey(t *testing.T) {
	var k keyedLock
	var inside atomic.Int32
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			defer k.lock(1)()
			assert.EqualValues(t, 1, inside.Add(1))
			time.Sleep(5 * time.Millisecond)
			inside.Add(-1)
		})
	}
	wg.Wait()
	assert.Empty(t, k.m, "entries are dropped when nobody waits")
}
