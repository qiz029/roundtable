package roundtable

import "testing"

func TestQuestionInterestDeltasWeightTagsAndTerms(t *testing.T) {
	deltas := questionInterestDeltas(
		"Backend route planning",
		"Backend systems need careful rollout planning.",
		[]string{"Backend", "release"},
		4,
		1,
	)

	if got := deltas["backend"]; got != 4 {
		t.Fatalf("backend delta = %v, want 4", got)
	}
	if got := deltas["release"]; got != 4 {
		t.Fatalf("release delta = %v, want 4", got)
	}
	if got := deltas["planning"]; got != 1 {
		t.Fatalf("planning delta = %v, want 1", got)
	}
}

func TestFeedInterestScoreExplainsPositiveMatches(t *testing.T) {
	score, reasons := feedInterestScore(feedQuestion{
		Title:   "Backend route planning",
		Body:    "How should systems roll out safely?",
		TagsRaw: `["backend"]`,
	}, map[string]float64{
		"backend": 2,
		"route":   1,
	})

	if score != 19 {
		t.Fatalf("score = %d, want 19", score)
	}
	if len(reasons) != 2 || reasons[0] != "matched_interest_tags" || reasons[1] != "matched_interest_terms" {
		t.Fatalf("reasons = %#v, want interest tag and term reasons", reasons)
	}
}

func TestFeedInterestScoreSuppressesNegativeMatchesWithoutReasons(t *testing.T) {
	score, reasons := feedInterestScore(feedQuestion{
		Title:   "Backend route planning",
		Body:    "How should systems roll out safely?",
		TagsRaw: `["backend"]`,
	}, map[string]float64{
		"backend": -8,
	})

	if score != -64 {
		t.Fatalf("score = %d, want -64", score)
	}
	if len(reasons) != 0 {
		t.Fatalf("reasons = %#v, want none for negative-only matches", reasons)
	}
}

func TestScoreFeedQuestionDoesNotReboostOpenedQuestionWithInterest(t *testing.T) {
	score, reasons := scoreFeedQuestion(feedQuestion{
		Title:     "Backend route planning",
		Body:      "How should systems roll out safely?",
		TagsRaw:   `["backend"]`,
		OpenCount: 1,
	}, currentUser{ID: "usr_123"}, true, feedSignals{
		AgentTerms: map[string]bool{},
		InterestTerms: map[string]float64{
			"backend": 8,
		},
	})

	if score >= 0 {
		t.Fatalf("score = %d, want opened question to stay demoted", score)
	}
	if containsString(reasons, "matched_interest_tags") {
		t.Fatalf("reasons = %#v, want no positive interest reason for opened question", reasons)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
