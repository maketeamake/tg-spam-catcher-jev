package profile

import (
	"fmt"
	"math"
)

// question names, the keys of jev answers
const (
	qClass     = "comment_class"
	qRelevance = "comment_relevance"
	qBait      = "bait"
	qAdult     = "profile_adult"
	qDrugs     = "profile_drugs"
	qScam      = "profile_scam"
	qPhoto     = "photo_provocative"

	classOK = "ok"
)

// Scores are the probabilities extracted from jev answers. Questions that were not asked stay zero.
type Scores struct {
	ClassAsked     bool
	Class          string  // the most probable class that is not ok
	ClassProb      float64 // probability of that class
	RelevanceAsked bool
	Relevance      float64 // 0 generic .. 1 specific
	Bait           float64
	Adult          float64
	Drugs          float64
	Scam           float64
	Photo          float64
}

// ProfileBad is the strongest sign of an advertising profile
func (s Scores) ProfileBad() float64 { return math.Max(s.Adult, math.Max(s.Drugs, s.Scam)) }

// Generic is true when the comment would fit under any post
func (s Scores) Generic() bool { return s.RelevanceAsked && s.Relevance <= 0.5 }

// String is a compact one-line form for admin cards and logs
func (s Scores) String() string {
	var out string
	if s.ClassAsked {
		out += fmt.Sprintf("class=%s %.2f ", s.Class, s.ClassProb)
	}
	if s.RelevanceAsked {
		out += fmt.Sprintf("rel=%.2f ", s.Relevance)
	}
	return out + fmt.Sprintf("bait=%.2f adult=%.2f drugs=%.2f scam=%.2f photo=%.2f", s.Bait, s.Adult, s.Drugs, s.Scam, s.Photo)
}

// parseScores extracts and validates the answers to the asked questions. A missing or malformed answer is an
// error, never a zero: a zero would read as a confident ham verdict.
func parseScores(resp Response, asked map[string]Question) (Scores, error) {
	var s Scores
	noul := func(name string) (float64, error) {
		a, ok := resp.Answers[name]
		if !ok || a.Type != "noul" || a.Noul == nil || math.IsNaN(*a.Noul) || *a.Noul < 0 || *a.Noul > 1 {
			return 0, fmt.Errorf("no valid noul answer for %q", name)
		}
		return *a.Noul, nil
	}
	for name := range asked {
		var err error
		switch name {
		case qClass:
			s.ClassAsked = true
			s.Class, s.ClassProb, err = bestSpamClass(resp.Answers[name])
		case qRelevance:
			s.RelevanceAsked = true
			levels, _ := asked[name].Criteria.([]string)
			s.Relevance, err = relevance(resp.Answers[name], len(levels))
		case qBait:
			s.Bait, err = noul(name)
		case qAdult:
			s.Adult, err = noul(name)
		case qDrugs:
			s.Drugs, err = noul(name)
		case qScam:
			s.Scam, err = noul(name)
		case qPhoto:
			s.Photo, err = noul(name)
		}
		if err != nil {
			return Scores{}, err
		}
	}
	return s, nil
}

func bestSpamClass(a Answer) (class string, prob float64, err error) {
	if a.Type != "choice" || len(a.Probabilities) == 0 {
		return "", 0, fmt.Errorf("no valid choice answer for %q", qClass)
	}
	for name, p := range a.Probabilities {
		if math.IsNaN(p) || p < 0 || p > 1.0001 {
			return "", 0, fmt.Errorf("class probability %v out of range", p)
		}
		if name != classOK && (p > prob || class == "") {
			class, prob = name, p
		}
	}
	return class, prob, nil
}

// relevance maps the weighted score 0..levels-1 onto 0..1
func relevance(a Answer, levels int) (float64, error) {
	if a.Type != "score" || a.Score == nil || math.IsNaN(*a.Score) || *a.Score < 0 || levels < 2 {
		return 0, fmt.Errorf("no valid score answer for %q", qRelevance)
	}
	return math.Min(*a.Score/float64(levels-1), 1), nil
}

// Decide applies the rules of the specification (section 5.3) to the scores. The returned reason names the
// rule that fired, or why the event passed, so calibration can see near misses.
func Decide(kind Kind, s Scores, th Thresholds) (action Action, reason string) {
	bad := s.ProfileBad()

	if s.ClassAsked && s.ClassProb >= th.CommentBan {
		return ActionBan, fmt.Sprintf("comment_class=%s %.2f >= %.2f", s.Class, s.ClassProb, th.CommentBan)
	}
	if bad >= th.ProfileBan {
		switch {
		case kind == KindReaction:
			return ActionBan, fmt.Sprintf("profile ads %.2f >= %.2f on reaction", bad, th.ProfileBan)
		case s.Generic():
			return ActionBan, fmt.Sprintf("profile ads %.2f >= %.2f with generic comment %.2f", bad, th.ProfileBan, s.Relevance)
		case s.Bait >= th.Bait:
			return ActionBan, fmt.Sprintf("profile ads %.2f >= %.2f with bait comment %.2f", bad, th.ProfileBan, s.Bait)
		case kind == KindJoin && s.Photo >= th.Photo:
			return ActionBan, fmt.Sprintf("profile ads %.2f >= %.2f with provocative photo %.2f", bad, th.ProfileBan, s.Photo)
		}
	}

	switch {
	case s.ClassAsked && s.ClassProb >= th.CommentReview:
		return ActionReview, fmt.Sprintf("comment_class=%s %.2f in [%.2f, %.2f)", s.Class, s.ClassProb, th.CommentReview, th.CommentBan)
	case bad >= th.ProfileReview: // an ad profile nothing backs up goes to a human, however high the score
		return ActionReview, fmt.Sprintf("profile ads %.2f >= %.2f", bad, th.ProfileReview)
	case s.Photo >= th.Photo && s.Generic():
		return ActionReview, fmt.Sprintf("provocative photo %.2f with generic comment %.2f", s.Photo, s.Relevance)
	}
	return ActionPass, "no rule fired"
}
