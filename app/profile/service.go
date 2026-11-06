package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	cache "github.com/go-pkgz/expirable-cache/v3"
	log "github.com/go-pkgz/lgr"
)

const (
	checkDeadline  = 10 * time.Second // hard cap of one check, jev has its own shorter timeout
	dossierTimeout = 2 * time.Second
	storeTimeout   = 5 * time.Second
	maxPostRunes   = 1500
	maxCacheUsers  = 20000
	foreverTTL     = 100 * 365 * 24 * time.Hour

	profilePhotosHint = ", avatars in `profile_photos`"
)

// CASFunc asks CAS if a user is a known spammer. An error means the answer is unknown, never "not a spammer".
type CASFunc func(ctx context.Context, userID int64) (spam bool, details string, err error)

type cachedVerdict struct {
	fingerprint string
	scores      Scores
	model       string
}

// Service checks events against the profile of the user
type Service struct {
	cfg   Config
	jev   *jevClient
	col   *collector
	store *Store
	cas   CASFunc

	dossiers cache.Cache[int64, *Dossier]
	verdicts cache.Cache[int64, cachedVerdict] // profile-only answers, reused for reactions and joins
	casCache cache.Cache[int64, bool]
	posts    cache.Cache[int, string]
	locks    keyedLock
}

// NewService makes the service. jevHTTP may be nil for a default client; cas may be nil to skip CAS.
func NewService(cfg Config, tg TG, store *Store, cas CASFunc, jevHTTP HTTPDoer) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if tg == nil || store == nil {
		return nil, errors.New("telegram api and store must be set")
	}
	if jevHTTP == nil {
		jevHTTP = &http.Client{}
	}
	jev, err := newJevClient(jevHTTP, cfg.Jev)
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg: cfg, jev: jev, store: store, cas: cas,
		col: &collector{tg: tg, http: &http.Client{Timeout: 10 * time.Second}, photos: cfg.PhotosLimit,
			posts: cfg.PersonalPostsLimit},
		dossiers: cache.NewCache[int64, *Dossier]().WithMaxKeys(maxCacheUsers).WithTTL(cfg.CacheTTL),
		verdicts: cache.NewCache[int64, cachedVerdict]().WithMaxKeys(maxCacheUsers).WithTTL(cfg.CacheTTL),
		casCache: cache.NewCache[int64, bool]().WithMaxKeys(maxCacheUsers * 5).WithTTL(foreverTTL),
		posts:    cache.NewCache[int, string]().WithMaxKeys(5000).WithTTL(cfg.CacheTTL),
	}, nil
}

// RememberPost keeps the text of a channel post forwarded to the discussion group, comments refer to it by message id
func (s *Service) RememberPost(msgID int, text string) {
	if text = trunc(text, maxPostRunes); text != "" {
		s.posts.Set(msgID, text, 0)
	}
}

// PostText returns the remembered text of the post with the message id, empty if unknown
func (s *Service) PostText(msgID int) string {
	text, _ := s.posts.Get(msgID)
	return text
}

// Check runs the whole check of an event and stores the decision. It never returns a sanction for a failed check.
// Checks of one user run one after another, so a burst reuses the first answer about the profile. An event without
// text (a reaction, a join) is skipped while another check of the same user runs: there is nothing of its own to
// judge or delete, and a burst of them must not occupy every worker.
func (s *Service) Check(ctx context.Context, ev Event) Result {
	start := time.Now()
	var unlock func()
	if ev.Kind == KindComment {
		unlock = s.locks.lock(ev.User.ID)
	} else {
		var ok bool
		if unlock, ok = s.locks.tryLock(ev.User.ID); !ok {
			return Result{Action: ActionSkip, Reason: "another check of this user is running"}
		}
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, checkDeadline)
	defer cancel()

	res := s.check(ctx, ev)
	res.Latency = time.Since(start)

	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer scancel()
	answers := ""
	if res.answers != nil {
		if b, err := json.Marshal(res.answers); err == nil {
			answers = string(b)
		}
	}
	id, err := s.store.AddDecision(sctx, Record{UserID: ev.User.ID, ChatID: ev.ChatID, MessageID: ev.MessageID,
		EventType: string(ev.Kind), CASHit: res.CASHit, JevModel: res.Model, JevAnswers: answers, TokensUsed: res.Tokens,
		Action: string(res.Action), Reason: res.Reason, LatencyMS: int(res.Latency.Milliseconds())})
	if err != nil {
		log.Printf("[WARN] profile: can't store decision for %d: %v", ev.User.ID, err)
		if res.Action == ActionBan || res.Action == ActionReview { // the card buttons would point to nothing
			res.Action, res.Err = ActionSkip, fmt.Errorf("decision not stored: %w", err)
		}
	}
	res.DecisionID = id
	return res.Result
}

// checked is Result plus the raw answers kept for storing
type checked struct {
	Result
	answers map[string]Answer
}

func (s *Service) check(ctx context.Context, ev Event) checked {
	res := checked{Result: Result{Action: ActionPass}}

	if ev.Kind != KindComment && s.cas != nil { // tg-spam already ran CAS on comments
		spam, details, err := s.casLookup(ctx, ev.User.ID)
		switch {
		case err != nil:
			log.Printf("[WARN] profile: cas lookup failed for %d: %v", ev.User.ID, err)
		case spam:
			res.CASHit, res.Action, res.Reason = true, ActionBan, "cas: "+details
			return res
		}
	}
	if ev.Kind == KindJoin && !s.cfg.CheckJoin {
		res.Reason = "profile check on join is off"
		return res
	}

	d := s.dossier(ctx, ev.User)
	res.Dossier = d
	commentText := strings.TrimSpace(ev.Comment)
	profileQ := !d.Empty()
	if commentText == "" && !profileQ {
		res.Reason = "nothing to judge"
		if d.Partial { // the profile was not read, an empty one would be a guess
			res.Action, res.Reason = ActionSkip, "profile could not be read"
		}
		return res
	}

	profileOnly := commentText == ""
	if profileOnly { // reuse the profile answers while the profile did not change
		if v, ok := s.verdicts.Get(ev.User.ID); ok && v.fingerprint == d.Fingerprint() {
			res.Scores, res.Model = v.scores, v.model
			res.Action, res.Reason = Decide(ev.Kind, v.scores, s.cfg.Thresholds)
			res.Reason += " (cached profile answers)"
			return res
		}
	}

	var images []Image
	var st state
	if profileQ && len(d.Photos) > 0 {
		for i, data := range s.col.download(ctx, d.Photos) {
			images = append(images, Image{Ref: fmt.Sprintf("@@profile-photo-%d@@", i), Data: data})
		}
	}
	questions := s.questions(ev, d, commentText != "", len(images) > 0)
	st = s.state(ev, d, commentText, images)

	resp, questions, err := s.ask(ctx, Request{State: st, Questions: questions, Images: images})
	if err != nil {
		res.Action, res.Err, res.Reason = ActionSkip, err, "jev: "+err.Error()
		return res
	}
	scores, err := parseScores(resp, questions)
	if err != nil {
		res.Action, res.Err, res.Reason = ActionSkip, err, "jev: "+err.Error()
		return res
	}
	res.Scores, res.Model, res.Tokens = scores, resp.Model, resp.Usage.InputTokens+resp.Usage.OutputTokens
	res.answers = resp.Answers
	res.Action, res.Reason = Decide(ev.Kind, scores, s.cfg.Thresholds)
	if profileOnly && len(images) == len(d.Photos) { // answers without some avatars must not stand for the full profile
		s.verdicts.Set(ev.User.ID, cachedVerdict{fingerprint: d.Fingerprint(), scores: scores, model: resp.Model}, 0)
	}
	return res
}

// ask sends the request and returns the answer with the questions that were actually asked. A rejected image request
// is repeated once without the images and the questions about them, a transport error is retried once.
func (s *Service) ask(ctx context.Context, r Request) (Response, map[string]Question, error) {
	resp, err := s.jev.ask(ctx, r)
	var he *HTTPError
	switch {
	case err == nil:
		return resp, r.Questions, nil
	case errors.As(err, &he) && len(r.Images) > 0 &&
		(he.Status == http.StatusBadRequest || he.Status == http.StatusRequestEntityTooLarge):
		log.Printf("[WARN] profile: jev rejected the request with images, retrying without them: %v", err)
		r = stripImages(r)
		resp, err = s.jev.ask(ctx, r)
		return resp, r.Questions, err
	case errors.As(err, &he) && he.Status < 500:
		return Response{}, nil, err
	}
	log.Printf("[DEBUG] profile: jev request failed, retrying once: %v", err)
	resp, err = s.jev.ask(ctx, r)
	return resp, r.Questions, err
}

// stripImages removes the images and the questions about them, and the references from the state
func stripImages(r Request) Request {
	q := make(map[string]Question, len(r.Questions))
	for name, question := range r.Questions {
		if name != qPhoto {
			question.Instructions = strings.ReplaceAll(question.Instructions, profilePhotosHint, "")
			q[name] = question
		}
	}
	st, _ := r.State.(state)
	st.ProfilePhotos = nil
	return Request{State: st, Questions: q}
}

// dossier returns the cached profile if it is still current, otherwise reads it again
func (s *Service) dossier(ctx context.Context, u User) *Dossier {
	dctx, cancel := context.WithTimeout(ctx, dossierTimeout)
	defer cancel()
	if d, ok := s.dossiers.Get(u.ID); ok && s.col.stillFresh(dctx, d, u) {
		return d
	}
	d := s.col.fetch(dctx, u)
	if !d.Partial { // a part missing because of a slow call is read again next time
		s.dossiers.Set(u.ID, d, 0)
	}
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer scancel()
	if err := s.store.SaveDossier(sctx, d); err != nil {
		log.Printf("[WARN] profile: %v", err)
	}
	return d
}

// casLookup answers from the cache when it can: a spammer is remembered forever, a miss for the negative ttl
func (s *Service) casLookup(ctx context.Context, userID int64) (spam bool, details string, err error) {
	if v, ok := s.casCache.Get(userID); ok {
		return v, "cached", nil
	}
	if spam, details, err = s.cas(ctx, userID); err != nil {
		return false, "", err // an unknown answer is not cached
	}
	ttl := s.cfg.CASNegativeTTL
	if spam {
		ttl = foreverTTL
	}
	s.casCache.Set(userID, spam, ttl)
	return spam, details, nil
}

// Resolve records the verdict of an admin on a decision. The bool is false if somebody answered before.
func (s *Service) Resolve(ctx context.Context, decisionID, adminID int64, v Verdict) (Record, bool, error) {
	rec, err := s.store.Decision(ctx, decisionID)
	if err != nil {
		return Record{}, false, err
	}
	first, err := s.store.AddReview(ctx, decisionID, adminID, v)
	if err != nil {
		return Record{}, false, err
	}
	if v == VerdictHam {
		s.verdicts.Invalidate(rec.UserID)
	}
	if d, ok := s.dossiers.Get(rec.UserID); ok {
		rec.UserName = d.User.Username
		if rec.UserName == "" {
			rec.UserName = strings.TrimSpace(d.User.FirstName + " " + d.User.LastName)
		}
	}
	return rec, first, nil
}

// Unresolve removes the verdict of a decision whose action failed, so a press of any card button acts again,
// also after a restart
func (s *Service) Unresolve(ctx context.Context, decisionID int64) error {
	return s.store.RemoveReview(ctx, decisionID)
}

// Abandon turns a review whose card could not be sent into a skip, together with the later unanswered reviews of the
// user it kept from getting a card, so none of them hides the cards of the next reviews
func (s *Service) Abandon(ctx context.Context, userID, decisionID int64, why string) error {
	return s.store.AbandonReviews(ctx, userID, decisionID, " ("+why+")")
}

// Cleared tells if an admin marked the user "not spam" since the moment, so a slower check must not sanction them
func (s *Service) Cleared(ctx context.Context, userID int64, since time.Time) bool {
	ok, err := s.store.AdminCleared(ctx, userID, since)
	if err != nil {
		log.Printf("[WARN] profile: can't check admin verdicts for %d, no sanction: %v", userID, err)
		return true
	}
	return ok
}

// HasOpenReview tells if the user already has an older review waiting for an admin. Reviews older than the review
// timeout are expired and do not count, so unanswered cards of a dry run do not hide new ones.
func (s *Service) HasOpenReview(ctx context.Context, userID, beforeDecisionID int64) bool {
	ok, err := s.store.HasOpenReview(ctx, userID, beforeDecisionID, time.Now().Add(-s.cfg.ReviewTimeout))
	if err != nil {
		log.Printf("[WARN] profile: can't check open reviews for %d: %v", userID, err)
	}
	return ok
}

// LogStats writes one line with the numbers of the last period
func (s *Service) LogStats(ctx context.Context, period, reviewTimeout time.Duration) {
	st, err := s.store.Stats(ctx, time.Now().Add(-period), reviewTimeout)
	if err != nil {
		log.Printf("[WARN] profile: can't read stats: %v", err)
		return
	}
	pct := func(n int) float64 {
		if st.Total == 0 {
			return 0
		}
		return 100 * float64(n) / float64(st.Total)
	}
	fp := 0.0
	if st.Ban > 0 {
		fp = 100 * float64(st.Unbans) / float64(st.Ban)
	}
	log.Printf("[INFO] profile stats for %s: checks=%d tokens=%d cost=$%.4f pass=%.0f%% review=%.0f%% ban=%.0f%% skip=%.0f%% "+
		"unbanned-after-ban=%.0f%% open-reviews=%d jev-latency p50=%v p95=%v", period, st.Total, st.Tokens,
		float64(st.Tokens)*jevPricePerToken, pct(st.Pass), pct(st.Review), pct(st.Ban), pct(st.Skip), fp, st.OpenReviews,
		st.LatencyP50, st.LatencyP95)
}

// jevPricePerToken is the input price of jev, $0.042 per million tokens
const jevPricePerToken = 0.042 / 1e6

// Run logs stats every interval until the context ends
func (s *Service) Run(ctx context.Context, interval, reviewTimeout time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.LogStats(ctx, interval, reviewTimeout)
		}
	}
}

// keyedLock serializes work per key, a second event of the same user waits for the first
type keyedLock struct {
	mu sync.Mutex
	m  map[int64]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

// lock takes the lock of the key and returns the function that releases it
func (k *keyedLock) lock(key int64) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[int64]*lockEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &lockEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return k.unlockFunc(key, e)
}

// tryLock takes the lock of the key only if nobody holds or waits for it
func (k *keyedLock) tryLock(key int64) (unlock func(), ok bool) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[int64]*lockEntry{}
	}
	if _, busy := k.m[key]; busy {
		k.mu.Unlock()
		return nil, false
	}
	e := &lockEntry{refs: 1}
	k.m[key] = e
	e.mu.Lock()
	k.mu.Unlock()
	return k.unlockFunc(key, e), true
}

func (k *keyedLock) unlockFunc(key int64, e *lockEntry) func() {
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
