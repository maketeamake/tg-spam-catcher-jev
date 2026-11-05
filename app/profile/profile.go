// Package profile checks the profile of a Telegram user (bio, personal channel, avatars) together with the
// comment or reaction the user produced, using the jev decision model. It builds a dossier of the user,
// asks jev several typed questions in one request and turns the returned probabilities into a decision
// (pass, review, ban) with thresholds kept in code.
package profile

import (
	"fmt"
	"math"
	"time"
)

// Kind is the type of event a user is checked for
type Kind string

// event kinds
const (
	KindComment  Kind = "comment"
	KindReaction Kind = "reaction"
	KindJoin     Kind = "join"
)

// Action is the outcome of a check
type Action string

// actions. ActionSkip means the check could not be completed (jev down, bad answer): nobody is punished for it
const (
	ActionPass   Action = "pass"
	ActionReview Action = "review"
	ActionBan    Action = "ban"
	ActionSkip   Action = "skip"
)

// Verdict is the decision of an admin on a flagged user
type Verdict string

// admin verdicts
const (
	VerdictSpam Verdict = "spam"
	VerdictHam  Verdict = "ham"
)

// User is the subset of Telegram user fields the check needs
type User struct {
	ID        int64
	FirstName string
	LastName  string
	Username  string
	IsPremium bool
}

// Event is a single thing to check: a comment, a reaction or a join
type Event struct {
	Kind      Kind
	User      User
	ChatID    int64
	MessageID int    // comment message or reacted message, 0 for join
	PostText  string // text of the channel post the comment belongs to
	Comment   string // comment text, kind comment only
	ReplyText string // text of the message the comment replies to, if it is not the post itself
}

// Result is what a check returns to the caller. DecisionID is 0 when nothing was stored.
type Result struct {
	DecisionID int64
	Action     Action
	Reason     string
	CASHit     bool
	Scores     Scores
	Model      string
	Tokens     int
	Latency    time.Duration
	Dossier    *Dossier
	Err        error // set for ActionSkip
}

// Thresholds are the probabilities the decision is made with. They are the tuned surface of the module:
// calibrate them on dry-run data before enabling bans.
type Thresholds struct {
	CommentBan    float64 // non-ok comment class at or above this bans
	CommentReview float64 // non-ok comment class at or above this goes to review
	ProfileBan    float64 // profile ads at or above this ban when the comment or event backs them up
	ProfileReview float64 // profile ads at or above this go to review
	Bait          float64 // bait comment probability that backs up a bad profile
	Photo         float64 // provocative avatar probability
}

// DefaultThresholds returns the starting values of the specification
func DefaultThresholds() Thresholds {
	return Thresholds{CommentBan: 0.90, CommentReview: 0.60, ProfileBan: 0.85, ProfileReview: 0.60, Bait: 0.70, Photo: 0.85}
}

// Validate checks that every threshold is a probability and that review levels do not exceed ban levels
func (t Thresholds) Validate() error {
	for name, v := range map[string]float64{"comment-ban": t.CommentBan, "comment-review": t.CommentReview,
		"profile-ban": t.ProfileBan, "profile-review": t.ProfileReview, "bait": t.Bait, "photo": t.Photo} {
		if math.IsNaN(v) || v <= 0 || v > 1 {
			return fmt.Errorf("threshold %s (%v) must be in (0, 1]", name, v)
		}
	}
	if t.CommentReview > t.CommentBan {
		return fmt.Errorf("comment-review threshold (%v) must not exceed comment-ban (%v)", t.CommentReview, t.CommentBan)
	}
	if t.ProfileReview > t.ProfileBan {
		return fmt.Errorf("profile-review threshold (%v) must not exceed profile-ban (%v)", t.ProfileReview, t.ProfileBan)
	}
	return nil
}

// Config is the full set of parameters of the Service
type Config struct {
	CacheTTL           time.Duration // dossier and profile verdict lifetime
	CASNegativeTTL     time.Duration // how long a "not in CAS" answer is trusted
	PhotosLimit        int           // avatars sent to jev
	PersonalPostsLimit int           // posts of the personal channel sent to jev
	CheckJoin          bool          // check profiles of new members
	ReviewTimeout      time.Duration // an unanswered review stops suppressing new cards after this time
	Jev                JevConfig
	Thresholds         Thresholds
	Criteria           *Criteria
}

// Validate checks the config and fills nothing in; use DefaultConfig for the starting values
func (c *Config) Validate() error {
	if c.CacheTTL <= 0 {
		return fmt.Errorf("profile cache ttl (%v) must be positive", c.CacheTTL)
	}
	if c.CASNegativeTTL <= 0 {
		return fmt.Errorf("profile cas negative ttl (%v) must be positive", c.CASNegativeTTL)
	}
	if c.PhotosLimit < 0 || c.PhotosLimit > 10 {
		return fmt.Errorf("profile photos limit (%d) must be in [0, 10]", c.PhotosLimit)
	}
	if c.PersonalPostsLimit < 0 || c.PersonalPostsLimit > 20 {
		return fmt.Errorf("profile personal posts limit (%d) must be in [0, 20]", c.PersonalPostsLimit)
	}
	if c.Criteria == nil {
		return fmt.Errorf("profile criteria must be set")
	}
	if err := c.Thresholds.Validate(); err != nil {
		return err
	}
	return c.Jev.validate()
}

// DefaultConfig returns the starting values of the specification with the built-in criteria
func DefaultConfig() (Config, error) {
	crit, err := LoadCriteria("")
	if err != nil {
		return Config{}, err
	}
	return Config{
		CacheTTL: 7 * 24 * time.Hour, CASNegativeTTL: DefaultCASNegativeTTL, PhotosLimit: 3, PersonalPostsLimit: 5, ReviewTimeout: 24 * time.Hour,
		Jev: JevConfig{Model: DefaultJevModel, Timeout: DefaultJevTimeout}, Thresholds: DefaultThresholds(), Criteria: crit,
	}, nil
}

// DefaultCASNegativeTTL is how long a "not in CAS" answer is trusted
const DefaultCASNegativeTTL = 24 * time.Hour
