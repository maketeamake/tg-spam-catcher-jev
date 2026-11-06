package profile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecide(t *testing.T) {
	th := DefaultThresholds()
	tests := []struct {
		name string
		kind Kind
		s    Scores
		want Action
	}{
		{"job scam comment with clean profile bans", KindComment, Scores{ClassAsked: true, Class: "job_scam", ClassProb: 0.97}, ActionBan},
		{"unsure scam class goes to review", KindComment, Scores{ClassAsked: true, Class: "adult", ClassProb: 0.7}, ActionReview},
		{"weak class passes", KindComment, Scores{ClassAsked: true, Class: "other_ad", ClassProb: 0.2}, ActionPass},
		{"escort profile with bait comment bans", KindComment, Scores{ClassAsked: true, Class: "other_ad", ClassProb: 0.1,
			RelevanceAsked: true, Relevance: 0.1, Bait: 0.8, Adult: 0.95, Photo: 0.9}, ActionBan},
		{"escort profile with generic comment bans", KindComment, Scores{RelevanceAsked: true, Relevance: 0.2, Adult: 0.9}, ActionBan},
		{"same account reacting bans", KindReaction, Scores{Adult: 0.95, Photo: 0.9}, ActionBan},
		{"escort profile with specific comment goes to review", KindComment,
			Scores{RelevanceAsked: true, Relevance: 0.9, Bait: 0.1, Adult: 0.95}, ActionReview},
		{"unknown relevance is not generic, no ban", KindComment, Scores{Adult: 0.95}, ActionReview},
		{"profile in the gray zone goes to review", KindReaction, Scores{Scam: 0.7}, ActionReview},
		{"join with ads and provocative photo bans", KindJoin, Scores{Drugs: 0.9, Photo: 0.9}, ActionBan},
		{"join with ads and no photo goes to review", KindJoin, Scores{Drugs: 0.9}, ActionReview},
		{"pretty avatar with template comment is not a ban", KindComment, Scores{RelevanceAsked: true, Relevance: 0.1, Bait: 0.3, Photo: 0.5}, ActionPass},
		{"provocative avatar with template comment goes to review", KindComment, Scores{RelevanceAsked: true, Relevance: 0.1, Photo: 0.9}, ActionReview},
		{"clean account with on-topic comment passes", KindComment, Scores{ClassAsked: true, Class: "other_ad", ClassProb: 0.01,
			RelevanceAsked: true, Relevance: 0.9}, ActionPass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := Decide(tt.kind, tt.s, th)
			assert.Equal(t, tt.want, got, reason)
			assert.NotEmpty(t, reason)
		})
	}
}

func TestParseScores(t *testing.T) {
	one, half := 1.0, 0.5
	asked := map[string]Question{qClass: {Type: "choice"}, qRelevance: {Type: "score", Criteria: []string{"a", "b", "c"}},
		qBait: {Type: "noul"}}

	t.Run("valid answers", func(t *testing.T) {
		resp := Response{Answers: map[string]Answer{
			qClass:     {Type: "choice", Probabilities: map[string]float64{"ok": 0.2, "job_scam": 0.7, "adult": 0.1}},
			qRelevance: {Type: "score", Score: &one}, // level 1 of 0..2 is 0.5 after normalising
			qBait:      {Type: "noul", Noul: &half},
		}}
		s, err := parseScores(resp, asked)
		require.NoError(t, err)
		assert.Equal(t, "job_scam", s.Class)
		assert.InDelta(t, 0.7, s.ClassProb, 1e-9)
		assert.InDelta(t, 0.5, s.Relevance, 1e-9)
		assert.True(t, s.Generic(), "0.5 is the border of generic")
		assert.InDelta(t, 0.5, s.Bait, 1e-9)
	})
	t.Run("missing answer is an error not a zero", func(t *testing.T) {
		_, err := parseScores(Response{Answers: map[string]Answer{qBait: {Type: "noul"}}}, map[string]Question{qBait: {Type: "noul"}})
		require.Error(t, err)
	})
	t.Run("out of range noul is an error", func(t *testing.T) {
		bad := 1.5
		_, err := parseScores(Response{Answers: map[string]Answer{qBait: {Type: "noul", Noul: &bad}}}, map[string]Question{qBait: {Type: "noul"}})
		require.Error(t, err)
	})
}

func TestThresholdsValidate(t *testing.T) {
	require.NoError(t, DefaultThresholds().Validate())
	th := DefaultThresholds()
	th.CommentReview = 0.95
	require.Error(t, th.Validate())
	th = DefaultThresholds()
	th.Bait = 0
	require.Error(t, th.Validate())
}

func TestCriteria(t *testing.T) {
	c, err := LoadCriteria("")
	require.NoError(t, err)
	assert.Contains(t, c.CommentClass.Classes, classOK)
	assert.Len(t, c.CommentRelevance.Levels, 3)
	assert.Contains(t, c.ProfileAdult.Instructions, profilePhotosHint, "the hint must match the wording so it can be dropped without photos")
	assert.NotContains(t, boolQuestion(c.ProfileAdult, false).Instructions, "profile_photos")
}
