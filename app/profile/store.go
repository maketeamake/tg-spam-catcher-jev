package profile

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/umputun/tg-spam/app/storage/engine"
)

// store commands
const (
	cmdCreateDossiers engine.DBCmd = iota + 700
	cmdCreateDecisions
	cmdCreateReviews
	cmdIndexDecisions
	cmdIndexDossiers
	cmdIndexReviews
	cmdSaveDossier
	cmdAddDecision
	cmdAddReview
	cmdGetDecision
	cmdOpenReviews
	cmdAdminCleared
	cmdStatsByAction
	cmdStatsUnbans
	cmdStatsLatency
	cmdStatsOpenReviews
	cmdExport
)

const decisionColumns = "id, user_id, chat_id, message_id, event_type, cas_hit, jev_model, jev_answers, tokens_used, " +
	"action, reason, latency_ms, created_at"

var storeQueries = engine.NewQueryMap().
	Add(cmdCreateDossiers, engine.Query{
		Sqlite: `CREATE TABLE IF NOT EXISTS profile_dossiers (
            gid TEXT NOT NULL, user_id INTEGER NOT NULL, fetched_at TIMESTAMP NOT NULL,
            photo_unique_ids TEXT NOT NULL DEFAULT '', json TEXT NOT NULL, PRIMARY KEY (gid, user_id))`,
		Postgres: `CREATE TABLE IF NOT EXISTS profile_dossiers (
            gid TEXT NOT NULL, user_id BIGINT NOT NULL, fetched_at TIMESTAMP NOT NULL,
            photo_unique_ids TEXT NOT NULL DEFAULT '', json TEXT NOT NULL, PRIMARY KEY (gid, user_id))`,
	}).
	Add(cmdCreateDecisions, engine.Query{
		Sqlite: `CREATE TABLE IF NOT EXISTS profile_decisions (
            id INTEGER PRIMARY KEY AUTOINCREMENT, gid TEXT NOT NULL, user_id INTEGER NOT NULL, chat_id INTEGER NOT NULL,
            message_id INTEGER NOT NULL DEFAULT 0, event_type TEXT NOT NULL, cas_hit BOOLEAN NOT NULL DEFAULT 0,
            jev_model TEXT NOT NULL DEFAULT '', jev_answers TEXT NOT NULL DEFAULT '', tokens_used INTEGER NOT NULL DEFAULT 0,
            action TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', latency_ms INTEGER NOT NULL DEFAULT 0,
            created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		Postgres: `CREATE TABLE IF NOT EXISTS profile_decisions (
            id SERIAL PRIMARY KEY, gid TEXT NOT NULL, user_id BIGINT NOT NULL, chat_id BIGINT NOT NULL,
            message_id INTEGER NOT NULL DEFAULT 0, event_type TEXT NOT NULL, cas_hit BOOLEAN NOT NULL DEFAULT FALSE,
            jev_model TEXT NOT NULL DEFAULT '', jev_answers TEXT NOT NULL DEFAULT '', tokens_used INTEGER NOT NULL DEFAULT 0,
            action TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '', latency_ms INTEGER NOT NULL DEFAULT 0,
            created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	}).
	Add(cmdCreateReviews, engine.Query{
		Sqlite: `CREATE TABLE IF NOT EXISTS profile_reviews (
            decision_id INTEGER PRIMARY KEY, gid TEXT NOT NULL, admin_id INTEGER NOT NULL, verdict TEXT NOT NULL,
            created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		Postgres: `CREATE TABLE IF NOT EXISTS profile_reviews (
            decision_id BIGINT PRIMARY KEY, gid TEXT NOT NULL, admin_id BIGINT NOT NULL, verdict TEXT NOT NULL,
            created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	}).
	AddSame(cmdIndexDecisions, `CREATE INDEX IF NOT EXISTS idx_profile_decisions_user ON profile_decisions(gid, user_id, created_at)`).
	AddSame(cmdIndexDossiers, `CREATE INDEX IF NOT EXISTS idx_profile_dossiers_fetched ON profile_dossiers(gid, fetched_at)`).
	AddSame(cmdIndexReviews, `CREATE INDEX IF NOT EXISTS idx_profile_reviews_created ON profile_reviews(gid, created_at)`).
	AddSame(cmdSaveDossier, `INSERT INTO profile_dossiers (gid, user_id, fetched_at, photo_unique_ids, json) VALUES (?, ?, ?, ?, ?)
        ON CONFLICT (gid, user_id) DO UPDATE SET fetched_at = excluded.fetched_at,
        photo_unique_ids = excluded.photo_unique_ids, json = excluded.json`).
	Add(cmdAddDecision, engine.Query{
		Sqlite: `INSERT INTO profile_decisions (gid, user_id, chat_id, message_id, event_type, cas_hit, jev_model, jev_answers,
            tokens_used, action, reason, latency_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		Postgres: `INSERT INTO profile_decisions (gid, user_id, chat_id, message_id, event_type, cas_hit, jev_model, jev_answers,
            tokens_used, action, reason, latency_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
	}).
	AddSame(cmdAddReview, `INSERT INTO profile_reviews (decision_id, gid, admin_id, verdict, created_at) VALUES (?, ?, ?, ?, ?)
        ON CONFLICT (decision_id) DO NOTHING`).
	AddSame(cmdGetDecision, `SELECT `+decisionColumns+` FROM profile_decisions WHERE gid = ? AND id = ?`).
	AddSame(cmdOpenReviews, `SELECT COUNT(*) FROM profile_decisions d LEFT JOIN profile_reviews r ON r.decision_id = d.id
        WHERE d.gid = ? AND d.user_id = ? AND d.id < ? AND d.created_at >= ? AND d.action = 'review' AND r.decision_id IS NULL`).
	AddSame(cmdAdminCleared, `SELECT COUNT(*) FROM profile_reviews r JOIN profile_decisions d ON d.id = r.decision_id
        WHERE d.gid = ? AND d.user_id = ? AND r.verdict = 'ham' AND r.created_at >= ?`).
	AddSame(cmdStatsByAction, `SELECT action, COUNT(*), COALESCE(SUM(tokens_used), 0) FROM profile_decisions
        WHERE gid = ? AND created_at >= ? GROUP BY action`).
	AddSame(cmdStatsUnbans, `SELECT COUNT(*) FROM profile_reviews r JOIN profile_decisions d ON d.id = r.decision_id
        WHERE d.gid = ? AND d.action = 'ban' AND r.verdict = 'ham' AND d.created_at >= ?`).
	AddSame(cmdStatsLatency, `SELECT latency_ms FROM profile_decisions WHERE gid = ? AND created_at >= ? AND latency_ms > 0`).
	AddSame(cmdStatsOpenReviews, `SELECT COUNT(*) FROM profile_decisions d LEFT JOIN profile_reviews r ON r.decision_id = d.id
        WHERE d.gid = ? AND d.action = 'review' AND r.decision_id IS NULL AND d.created_at < ?`).
	AddSame(cmdExport, `SELECT d.id, d.user_id, d.chat_id, d.message_id, d.event_type, d.cas_hit, d.jev_model, d.jev_answers,
        d.tokens_used, d.action, d.reason, d.latency_ms, d.created_at, COALESCE(r.verdict, ''), COALESCE(r.admin_id, 0),
        r.created_at FROM profile_decisions d LEFT JOIN profile_reviews r ON r.decision_id = d.id
        WHERE d.gid = ? ORDER BY d.id`)

// Record is a stored decision
type Record struct {
	ID         int64     `db:"id"`
	UserID     int64     `db:"user_id"`
	ChatID     int64     `db:"chat_id"`
	MessageID  int       `db:"message_id"`
	EventType  string    `db:"event_type"`
	CASHit     bool      `db:"cas_hit"`
	JevModel   string    `db:"jev_model"`
	JevAnswers string    `db:"jev_answers"`
	TokensUsed int       `db:"tokens_used"`
	Action     string    `db:"action"`
	Reason     string    `db:"reason"`
	LatencyMS  int       `db:"latency_ms"`
	CreatedAt  time.Time `db:"created_at"`
}

// Stats summarises decisions since a moment
type Stats struct {
	Total, Pass, Review, Ban, Skip int
	Tokens                         int
	Unbans                         int // auto-bans an admin reverted
	OpenReviews                    int // reviews waiting longer than the review timeout
	LatencyP50, LatencyP95         time.Duration
}

// Store keeps dossiers, decisions and admin reviews
type Store struct {
	*engine.SQL
	engine.RWLocker
}

// NewStore creates the tables if needed
func NewStore(ctx context.Context, db *engine.SQL) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("db connection is nil")
	}
	s := &Store{SQL: db, RWLocker: db.MakeLock()}
	tables := []struct {
		name          string
		create, index engine.DBCmd
	}{
		{"profile_dossiers", cmdCreateDossiers, cmdIndexDossiers},
		{"profile_decisions", cmdCreateDecisions, cmdIndexDecisions},
		{"profile_reviews", cmdCreateReviews, cmdIndexReviews},
	}
	for _, t := range tables {
		cfg := engine.TableConfig{Name: t.name, CreateTable: t.create, CreateIndexes: t.index,
			MigrateFunc: func(context.Context, *sqlx.Tx, string) error { return nil }, QueriesMap: storeQueries}
		if err := engine.InitTable(ctx, db, cfg); err != nil {
			return nil, fmt.Errorf("failed to init %s: %w", t.name, err)
		}
	}
	return s, nil
}

func (s *Store) query(cmd engine.DBCmd) (string, error) {
	q, err := storeQueries.Pick(s.Type(), cmd)
	if err != nil {
		return "", fmt.Errorf("failed to get query: %w", err)
	}
	return s.Adopt(q), nil
}

// SaveDossier stores the profile without any picture, only avatar unique ids
func (s *Store) SaveDossier(ctx context.Context, d *Dossier) error {
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("can't marshal dossier: %w", err)
	}
	q, err := s.query(cmdSaveDossier)
	if err != nil {
		return err
	}
	s.Lock()
	defer s.Unlock()
	if _, err = s.ExecContext(ctx, q, s.GID(), d.User.ID, d.FetchedAt, strings.Join(d.PhotoIDs(), ","), string(data)); err != nil {
		return fmt.Errorf("failed to save dossier: %w", err)
	}
	return nil
}

// AddDecision stores a decision and returns its id
func (s *Store) AddDecision(ctx context.Context, r Record) (int64, error) {
	q, err := s.query(cmdAddDecision)
	if err != nil {
		return 0, err
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	args := []any{s.GID(), r.UserID, r.ChatID, r.MessageID, r.EventType, r.CASHit, r.JevModel, r.JevAnswers, r.TokensUsed,
		r.Action, r.Reason, r.LatencyMS, r.CreatedAt}
	s.Lock()
	defer s.Unlock()
	if s.Type() == engine.Postgres {
		var id int64
		if err = s.QueryRowxContext(ctx, q, args...).Scan(&id); err != nil {
			return 0, fmt.Errorf("failed to add decision: %w", err)
		}
		return id, nil
	}
	res, err := s.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("failed to add decision: %w", err)
	}
	return res.LastInsertId() //nolint:wrapcheck // sqlite driver error is self-explanatory
}

// Decision returns a stored decision
func (s *Store) Decision(ctx context.Context, id int64) (Record, error) {
	q, err := s.query(cmdGetDecision)
	if err != nil {
		return Record{}, err
	}
	s.RLock()
	defer s.RUnlock()
	var r Record
	if err = s.GetContext(ctx, &r, q, s.GID(), id); err != nil {
		return Record{}, fmt.Errorf("failed to get decision %d: %w", id, err)
	}
	return r, nil
}

// AddReview records the verdict of an admin. It returns false if the decision was already reviewed.
func (s *Store) AddReview(ctx context.Context, decisionID, adminID int64, v Verdict) (bool, error) {
	q, err := s.query(cmdAddReview)
	if err != nil {
		return false, err
	}
	s.Lock()
	defer s.Unlock()
	res, err := s.ExecContext(ctx, q, decisionID, s.GID(), adminID, string(v), time.Now())
	if err != nil {
		return false, fmt.Errorf("failed to add review: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err //nolint:wrapcheck // driver error is self-explanatory
}

// HasOpenReview tells if an older review decision of the user, made since the moment, has no answer yet. Only older ones
// count, so of two decisions made together exactly the first one gets a card.
func (s *Store) HasOpenReview(ctx context.Context, userID, beforeDecisionID int64, since time.Time) (bool, error) {
	return s.count(ctx, cmdOpenReviews, s.GID(), userID, beforeDecisionID, since)
}

// AdminCleared tells if an admin marked the user "not spam" since the moment
func (s *Store) AdminCleared(ctx context.Context, userID int64, since time.Time) (bool, error) {
	return s.count(ctx, cmdAdminCleared, s.GID(), userID, since)
}

func (s *Store) count(ctx context.Context, cmd engine.DBCmd, args ...any) (bool, error) {
	q, err := s.query(cmd)
	if err != nil {
		return false, err
	}
	s.RLock()
	defer s.RUnlock()
	var n int
	if err = s.GetContext(ctx, &n, q, args...); err != nil {
		return false, fmt.Errorf("failed to count: %w", err)
	}
	return n > 0, nil
}

// Stats summarises decisions since the moment; reviews older than reviewTimeout count as open
func (s *Store) Stats(ctx context.Context, since time.Time, reviewTimeout time.Duration) (Stats, error) {
	var st Stats
	s.RLock()
	defer s.RUnlock()

	q, err := s.query(cmdStatsByAction)
	if err != nil {
		return st, err
	}
	rows, err := s.QueryxContext(ctx, q, s.GID(), since)
	if err != nil {
		return st, fmt.Errorf("failed to read stats: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only rows
	for rows.Next() {
		var action string
		var n, tokens int
		if err = rows.Scan(&action, &n, &tokens); err != nil {
			return st, fmt.Errorf("failed to scan stats: %w", err)
		}
		st.Total += n
		st.Tokens += tokens
		switch Action(action) {
		case ActionPass:
			st.Pass = n
		case ActionReview:
			st.Review = n
		case ActionBan:
			st.Ban = n
		case ActionSkip:
			st.Skip = n
		}
	}
	if err = rows.Err(); err != nil {
		return st, fmt.Errorf("failed to read stats: %w", err)
	}

	for cmd, dst := range map[engine.DBCmd]*int{cmdStatsUnbans: &st.Unbans, cmdStatsOpenReviews: &st.OpenReviews} {
		if q, err = s.query(cmd); err != nil {
			return st, err
		}
		arg := since
		if cmd == cmdStatsOpenReviews {
			arg = time.Now().Add(-reviewTimeout)
		}
		if err = s.GetContext(ctx, dst, q, s.GID(), arg); err != nil {
			return st, fmt.Errorf("failed to read stats: %w", err)
		}
	}

	if q, err = s.query(cmdStatsLatency); err != nil {
		return st, err
	}
	var lat []int
	if err = s.SelectContext(ctx, &lat, q, s.GID(), since); err != nil {
		return st, fmt.Errorf("failed to read latency: %w", err)
	}
	sort.Ints(lat)
	if len(lat) > 0 {
		st.LatencyP50 = time.Duration(lat[len(lat)/2]) * time.Millisecond
		st.LatencyP95 = time.Duration(lat[len(lat)*95/100]) * time.Millisecond
	}
	return st, nil
}

// ExportCSV writes every decision with the verdict of its review. reviewTimeout labels unanswered reviews "expired".
func (s *Store) ExportCSV(ctx context.Context, w io.Writer, reviewTimeout time.Duration) error {
	q, err := s.query(cmdExport)
	if err != nil {
		return err
	}
	s.RLock()
	defer s.RUnlock()
	rows, err := s.QueryxContext(ctx, q, s.GID())
	if err != nil {
		return fmt.Errorf("failed to export: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only rows

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "user_id", "chat_id", "message_id", "event_type", "cas_hit", "jev_model", "jev_answers",
		"tokens_used", "action", "reason", "latency_ms", "created_at", "verdict", "admin_id", "reviewed_at", "review_state"})
	for rows.Next() {
		var r Record
		var verdict string
		var adminID int64
		var reviewedAt *time.Time
		if err = rows.Scan(&r.ID, &r.UserID, &r.ChatID, &r.MessageID, &r.EventType, &r.CASHit, &r.JevModel, &r.JevAnswers,
			&r.TokensUsed, &r.Action, &r.Reason, &r.LatencyMS, &r.CreatedAt, &verdict, &adminID, &reviewedAt); err != nil {
			return fmt.Errorf("failed to scan export row: %w", err)
		}
		state, at := "", ""
		if reviewedAt != nil {
			state, at = "reviewed", reviewedAt.Format(time.RFC3339)
		} else if Action(r.Action) == ActionReview {
			state = "open"
			if time.Since(r.CreatedAt) > reviewTimeout {
				state = "expired"
			}
		}
		_ = cw.Write([]string{strconv.FormatInt(r.ID, 10), strconv.FormatInt(r.UserID, 10), strconv.FormatInt(r.ChatID, 10),
			strconv.Itoa(r.MessageID), r.EventType, strconv.FormatBool(r.CASHit), r.JevModel, r.JevAnswers,
			strconv.Itoa(r.TokensUsed), r.Action, r.Reason, strconv.Itoa(r.LatencyMS), r.CreatedAt.Format(time.RFC3339),
			verdict, strconv.FormatInt(adminID, 10), at, state})
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("failed to export: %w", err)
	}
	cw.Flush()
	return cw.Error() //nolint:wrapcheck // csv writer error is self-explanatory
}
