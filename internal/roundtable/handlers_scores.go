package roundtable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"
)

type monthlyScorePeriod struct {
	Period   string
	Status   string
	StartsAt string
	EndsAt   string
	FrozenAt sql.NullString
}

type agentScoreInfo struct {
	ID        string
	Name      string
	OwnerID   string
	OwnerName string
}

type scoredAnswer struct {
	ID            string
	AgentID       string
	OwnerUserID   string
	ViaInvitation bool
	CreatedAt     string
}

type scoredVote struct {
	AnswerID  string
	VoterType string
	UserID    string
	AgentID   string
	CreatedAt string
	Weight    float64
	Eligible  bool
}

type agentScoreAccumulator struct {
	Info             agentScoreInfo
	AnswerScore      float64
	CurationScore    float64
	ReliabilityScore float64
	PenaltyScore     float64
	TotalScore       float64
	Rank             int
	AnswerCount      int
	CurationHits     int
	SameOwnerLikes   int
}

type userScoreAccumulator struct {
	UserID           string
	DisplayName      string
	OwnedAgentScore  float64
	OperatorBonus    float64
	PenaltyScore     float64
	TotalScore       float64
	Rank             int
	Contributing     int
	TopAgentID       string
	TopAgentName     string
	TopAgentScore    float64
	WeightedBreakout []map[string]any
}

func (a *App) handleAgentLeaderboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	period, err := a.monthlyScorePeriodFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.ensureMonthlyScores(r.Context(), period); err != nil {
		writeError(w, err)
		return
	}
	a.writeAgentLeaderboard(w, r, period.Period)
}

func (a *App) handleUserLeaderboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	period, err := a.monthlyScorePeriodFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.ensureMonthlyScores(r.Context(), period); err != nil {
		writeError(w, err)
		return
	}
	a.writeUserLeaderboard(w, r, period.Period)
}

func (a *App) handleMyRewards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	user, err := a.requireUserFor(r.Context(), r, "read rewards")
	if err != nil {
		writeError(w, err)
		return
	}
	period, err := a.monthlyScorePeriodFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.ensureMonthlyScores(r.Context(), period); err != nil {
		writeError(w, err)
		return
	}
	a.writeMyRewards(w, r, period.Period, user.ID)
}

func (a *App) handlePublicAgentScore(w http.ResponseWriter, r *http.Request) {
	agentID, action, ok := twoPartAction(r.URL.Path, "/api/v1/agents/")
	if !ok || action != "scores" {
		writeError(w, errNotFound("agent action not found"))
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	period, err := a.monthlyScorePeriodFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.ensureMonthlyScores(r.Context(), period); err != nil {
		writeError(w, err)
		return
	}
	score, err := a.agentScoreResponse(r.Context(), period.Period, agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("agent score not found"))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, score)
}

func (a *App) handleUserScore(w http.ResponseWriter, r *http.Request, userID string) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	period, err := a.monthlyScorePeriodFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.ensureMonthlyScores(r.Context(), period); err != nil {
		writeError(w, err)
		return
	}
	score, err := a.userScoreResponse(r.Context(), period.Period, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("user score not found"))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, score)
}

func (a *App) monthlyScorePeriodFromRequest(r *http.Request) (monthlyScorePeriod, error) {
	period := r.URL.Query().Get("period")
	if period == "" {
		period = a.now().UTC().Format("2006-01")
	}
	start, err := time.Parse("2006-01", period)
	if err != nil {
		return monthlyScorePeriod{}, errInvalidInput("period must use YYYY-MM")
	}
	end := start.AddDate(0, 1, 0)
	return monthlyScorePeriod{
		Period:   period,
		Status:   "open",
		StartsAt: start.UTC().Format(time.RFC3339Nano),
		EndsAt:   end.UTC().Format(time.RFC3339Nano),
	}, nil
}

func (a *App) ensureMonthlyScores(ctx context.Context, period monthlyScorePeriod) error {
	existing, found, err := a.scorePeriod(ctx, period.Period)
	if err != nil {
		return err
	}
	if found && (existing.Status == "frozen" || existing.Status == "paid") {
		return nil
	}
	return a.calculateMonthlyScores(ctx, period)
}

func (a *App) scorePeriod(ctx context.Context, period string) (monthlyScorePeriod, bool, error) {
	var p monthlyScorePeriod
	err := a.db.QueryRowContext(ctx, `
		SELECT period, status, starts_at, ends_at, frozen_at
		FROM score_periods
		WHERE period = $1
	`, period).Scan(&p.Period, &p.Status, &p.StartsAt, &p.EndsAt, &p.FrozenAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return monthlyScorePeriod{}, false, nil
		}
		return monthlyScorePeriod{}, false, err
	}
	return p, true, nil
}

func (a *App) calculateMonthlyScores(ctx context.Context, period monthlyScorePeriod) error {
	agents, err := a.loadScoreAgents(ctx)
	if err != nil {
		return err
	}
	answers, err := a.loadScoreAnswers(ctx, period)
	if err != nil {
		return err
	}
	votes, err := a.loadScoreVotes(ctx, period, agents, answers)
	if err != nil {
		return err
	}
	agentScores := a.scoreAgents(agents, answers, votes)
	userScores, err := a.scoreUsers(ctx, agentScores)
	if err != nil {
		return err
	}
	return a.replaceMonthlyScores(ctx, period, agentScores, userScores)
}

func (a *App) loadScoreAgents(ctx context.Context) (map[string]agentScoreInfo, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT ag.id, ag.name, ag.owner_user_id, owner.display_name
		FROM agents ag
		JOIN users owner ON owner.id = ag.owner_user_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	agents := map[string]agentScoreInfo{}
	for rows.Next() {
		var info agentScoreInfo
		if err := rows.Scan(&info.ID, &info.Name, &info.OwnerID, &info.OwnerName); err != nil {
			return nil, err
		}
		agents[info.ID] = info
	}
	return agents, rows.Err()
}

func (a *App) loadScoreAnswers(ctx context.Context, period monthlyScorePeriod) (map[string]scoredAnswer, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT ans.id, ans.agent_id, ag.owner_user_id, ans.invitation_id IS NOT NULL, ans.created_at
		FROM answers ans
		JOIN agents ag ON ag.id = ans.agent_id
		WHERE ans.created_at >= $1 AND ans.created_at < $2
	`, period.StartsAt, period.EndsAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	answers := map[string]scoredAnswer{}
	for rows.Next() {
		var answer scoredAnswer
		if err := rows.Scan(&answer.ID, &answer.AgentID, &answer.OwnerUserID, &answer.ViaInvitation, &answer.CreatedAt); err != nil {
			return nil, err
		}
		answers[answer.ID] = answer
	}
	return answers, rows.Err()
}

func (a *App) loadScoreVotes(ctx context.Context, period monthlyScorePeriod, agents map[string]agentScoreInfo, answers map[string]scoredAnswer) (map[string][]scoredVote, error) {
	if len(answers) == 0 {
		return map[string][]scoredVote{}, nil
	}
	rows, err := a.db.QueryContext(ctx, `
		SELECT v.answer_id, v.voter_type, COALESCE(v.user_id, ''), COALESCE(v.agent_id, ''), v.created_at
		FROM votes v
		JOIN answers ans ON ans.id = v.answer_id
		WHERE v.revoked_at IS NULL
			AND v.created_at < $1
	`, period.EndsAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	votes := map[string][]scoredVote{}
	for rows.Next() {
		var vote scoredVote
		if err := rows.Scan(&vote.AnswerID, &vote.VoterType, &vote.UserID, &vote.AgentID, &vote.CreatedAt); err != nil {
			return nil, err
		}
		answer, ok := answers[vote.AnswerID]
		if !ok {
			continue
		}
		vote.Weight, vote.Eligible = scoreVoteWeight(agents, answer, vote)
		votes[vote.AnswerID] = append(votes[vote.AnswerID], vote)
	}
	for answerID := range votes {
		sort.Slice(votes[answerID], func(i, j int) bool {
			return votes[answerID][i].CreatedAt < votes[answerID][j].CreatedAt
		})
	}
	return votes, rows.Err()
}

func scoreVoteWeight(agents map[string]agentScoreInfo, answer scoredAnswer, vote scoredVote) (float64, bool) {
	if vote.VoterType == "user" {
		if vote.UserID == "" || vote.UserID == answer.OwnerUserID {
			return 0, false
		}
		return 3, true
	}
	if vote.VoterType != "agent" || vote.AgentID == "" {
		return 0, false
	}
	voter, ok := agents[vote.AgentID]
	if !ok {
		return 0, false
	}
	if voter.OwnerID == answer.OwnerUserID {
		return 0, false
	}
	return 1, true
}

func (a *App) scoreAgents(agents map[string]agentScoreInfo, answers map[string]scoredAnswer, votes map[string][]scoredVote) []agentScoreAccumulator {
	accumulators := map[string]*agentScoreAccumulator{}
	for agentID, info := range agents {
		info := info
		accumulators[agentID] = &agentScoreAccumulator{Info: info}
	}

	answerQuality := map[string]float64{}
	for answerID, answerVotes := range votes {
		for _, vote := range answerVotes {
			if vote.Eligible {
				answerQuality[answerID] += vote.Weight
			}
		}
	}

	for _, answer := range answers {
		acc := accumulators[answer.AgentID]
		if acc == nil {
			continue
		}
		acc.AnswerScore += answerQuality[answer.ID]
		acc.AnswerCount++
		if answer.ViaInvitation {
			acc.ReliabilityScore += 0.5
		}
	}

	for answerID, answerVotes := range votes {
		answer, ok := answers[answerID]
		if !ok {
			continue
		}
		finalQuality := answerQuality[answerID]
		var earlierQuality float64
		for _, vote := range answerVotes {
			if vote.VoterType == "agent" {
				acc := accumulators[vote.AgentID]
				if acc != nil && !vote.Eligible {
					acc.PenaltyScore += 2
					acc.SameOwnerLikes++
				}
				if acc != nil && vote.Eligible && finalQuality > 0 {
					acc.CurationScore += finalQuality * earlySignalMultiplier(earlierQuality, finalQuality) * 0.5
					acc.CurationHits++
				}
			}
			if vote.Eligible && vote.AnswerID == answer.ID {
				earlierQuality += vote.Weight
			}
		}
	}

	scored := make([]agentScoreAccumulator, 0, len(accumulators))
	for _, acc := range accumulators {
		acc.TotalScore = acc.AnswerScore + acc.CurationScore + acc.ReliabilityScore - acc.PenaltyScore
		if acc.TotalScore == 0 && acc.AnswerCount == 0 && acc.CurationHits == 0 && acc.SameOwnerLikes == 0 {
			continue
		}
		scored = append(scored, *acc)
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].TotalScore == scored[j].TotalScore {
			return scored[i].Info.ID < scored[j].Info.ID
		}
		return scored[i].TotalScore > scored[j].TotalScore
	})
	for i := range scored {
		scored[i].Rank = i + 1
	}
	return scored
}

func earlySignalMultiplier(earlierQuality float64, finalQuality float64) float64 {
	if finalQuality <= 0 {
		return 0
	}
	ratio := earlierQuality / finalQuality
	if ratio < 0.25 {
		return 1.5
	}
	if ratio < 0.75 {
		return 1.0
	}
	return 0.3
}

func (a *App) scoreUsers(ctx context.Context, agentScores []agentScoreAccumulator) ([]userScoreAccumulator, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id, display_name FROM users WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := map[string]*userScoreAccumulator{}
	for rows.Next() {
		var userID, displayName string
		if err := rows.Scan(&userID, &displayName); err != nil {
			return nil, err
		}
		users[userID] = &userScoreAccumulator{UserID: userID, DisplayName: displayName}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	byOwner := map[string][]agentScoreAccumulator{}
	for _, score := range agentScores {
		byOwner[score.Info.OwnerID] = append(byOwner[score.Info.OwnerID], score)
	}
	for ownerID, scores := range byOwner {
		userScore := users[ownerID]
		if userScore == nil {
			continue
		}
		sort.Slice(scores, func(i, j int) bool {
			return scores[i].TotalScore > scores[j].TotalScore
		})
		for i, score := range scores {
			weight := portfolioWeight(i)
			if weight <= 0 {
				continue
			}
			contribution := score.TotalScore * weight
			userScore.OwnedAgentScore += contribution
			userScore.Contributing++
			userScore.WeightedBreakout = append(userScore.WeightedBreakout, map[string]any{
				"agent_id":     score.Info.ID,
				"agent_name":   score.Info.Name,
				"agent_score":  score.TotalScore,
				"weight":       weight,
				"contribution": contribution,
			})
			if i == 0 {
				userScore.TopAgentID = score.Info.ID
				userScore.TopAgentName = score.Info.Name
				userScore.TopAgentScore = score.TotalScore
			}
		}
		userScore.TotalScore = userScore.OwnedAgentScore + userScore.OperatorBonus - userScore.PenaltyScore
	}

	scored := make([]userScoreAccumulator, 0, len(users))
	for _, score := range users {
		if score.TotalScore == 0 && score.Contributing == 0 {
			continue
		}
		scored = append(scored, *score)
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].TotalScore == scored[j].TotalScore {
			return scored[i].UserID < scored[j].UserID
		}
		return scored[i].TotalScore > scored[j].TotalScore
	})
	for i := range scored {
		scored[i].Rank = i + 1
	}
	return scored, nil
}

func portfolioWeight(index int) float64 {
	switch index {
	case 0:
		return 1
	case 1:
		return 0.5
	case 2:
		return 0.25
	default:
		return 0.1
	}
}

func (a *App) replaceMonthlyScores(ctx context.Context, period monthlyScorePeriod, agentScores []agentScoreAccumulator, userScores []userScoreAccumulator) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO score_periods (period, status, starts_at, ends_at)
		VALUES ($1, 'open', $2, $3)
		ON CONFLICT (period) DO UPDATE
		SET starts_at = EXCLUDED.starts_at,
			ends_at = EXCLUDED.ends_at
	`, period.Period, period.StartsAt, period.EndsAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_monthly_scores WHERE period = $1`, period.Period); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_monthly_scores WHERE period = $1`, period.Period); err != nil {
		return err
	}
	for _, score := range agentScores {
		details, err := json.Marshal(map[string]any{
			"answer_count":     score.AnswerCount,
			"curation_hits":    score.CurationHits,
			"same_owner_likes": score.SameOwnerLikes,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_monthly_scores (
				period, agent_id, owner_user_id, answer_score, curation_score,
				reliability_score, penalty_score, total_score, rank, details_json
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, period.Period, score.Info.ID, score.Info.OwnerID, score.AnswerScore, score.CurationScore,
			score.ReliabilityScore, score.PenaltyScore, score.TotalScore, score.Rank, string(details)); err != nil {
			return err
		}
	}
	for _, score := range userScores {
		details, err := json.Marshal(map[string]any{
			"contributing_agents": score.Contributing,
			"top_agent_id":        score.TopAgentID,
			"top_agent_name":      score.TopAgentName,
			"top_agent_score":     score.TopAgentScore,
			"portfolio":           score.WeightedBreakout,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_monthly_scores (
				period, user_id, owned_agent_score, operator_bonus, penalty_score,
				total_score, rank, details_json
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, period.Period, score.UserID, score.OwnedAgentScore, score.OperatorBonus, score.PenaltyScore,
			score.TotalScore, score.Rank, string(details)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) writeAgentLeaderboard(w http.ResponseWriter, r *http.Request, period string) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT s.agent_id, ag.name, s.owner_user_id, owner.display_name,
			s.answer_score, s.curation_score, s.reliability_score, s.penalty_score,
			s.total_score, s.rank, s.details_json
		FROM agent_monthly_scores s
		JOIN agents ag ON ag.id = s.agent_id
		JOIN users owner ON owner.id = s.owner_user_id
		WHERE s.period = $1
		ORDER BY s.rank ASC
		LIMIT $2 OFFSET $3
	`, period, page.Limit+1, page.Offset)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		item, err := scanAgentScore(rows, period)
		if err != nil {
			writeError(w, err)
			return
		}
		items = append(items, item)
	}
	items, hasMore := trimPaginatedItems(items, page)
	if err := rows.Err(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period":     period,
		"status":     "open",
		"items":      items,
		"pagination": paginationResponse(page, len(items), hasMore),
	})
}

func (a *App) writeUserLeaderboard(w http.ResponseWriter, r *http.Request, period string) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT s.user_id, u.display_name, s.owned_agent_score, s.operator_bonus,
			s.penalty_score, s.total_score, s.rank, s.details_json
		FROM user_monthly_scores s
		JOIN users u ON u.id = s.user_id
		WHERE s.period = $1
		ORDER BY s.rank ASC
		LIMIT $2 OFFSET $3
	`, period, page.Limit+1, page.Offset)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		item, err := scanUserScore(rows, period)
		if err != nil {
			writeError(w, err)
			return
		}
		items = append(items, item)
	}
	items, hasMore := trimPaginatedItems(items, page)
	if err := rows.Err(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period":     period,
		"status":     "open",
		"items":      items,
		"pagination": paginationResponse(page, len(items), hasMore),
	})
}

func (a *App) writeMyRewards(w http.ResponseWriter, r *http.Request, period string, userID string) {
	score, err := a.userScoreResponse(r.Context(), period, userID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		score = map[string]any{
			"period": period,
			"user": map[string]any{
				"id": userID,
			},
			"rank":              nil,
			"owned_agent_score": 0,
			"operator_bonus":    0,
			"penalty_score":     0,
			"total_score":       0,
			"details":           map[string]any{},
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"period": period,
		"status": "open",
		"score":  score,
	})
}

func (a *App) agentScoreResponse(ctx context.Context, period string, agentID string) (map[string]any, error) {
	row := a.db.QueryRowContext(ctx, `
		SELECT s.agent_id, ag.name, s.owner_user_id, owner.display_name,
			s.answer_score, s.curation_score, s.reliability_score, s.penalty_score,
			s.total_score, s.rank, s.details_json
		FROM agent_monthly_scores s
		JOIN agents ag ON ag.id = s.agent_id
		JOIN users owner ON owner.id = s.owner_user_id
		WHERE s.period = $1 AND s.agent_id = $2
	`, period, agentID)
	return scanAgentScore(row, period)
}

func (a *App) userScoreResponse(ctx context.Context, period string, userID string) (map[string]any, error) {
	row := a.db.QueryRowContext(ctx, `
		SELECT s.user_id, u.display_name, s.owned_agent_score, s.operator_bonus,
			s.penalty_score, s.total_score, s.rank, s.details_json
		FROM user_monthly_scores s
		JOIN users u ON u.id = s.user_id
		WHERE s.period = $1 AND s.user_id = $2
	`, period, userID)
	return scanUserScore(row, period)
}

type scoreScanner interface {
	Scan(dest ...any) error
}

func scanAgentScore(row scoreScanner, period string) (map[string]any, error) {
	var agentID, agentName, ownerID, ownerName, detailsRaw string
	var answerScore, curationScore, reliabilityScore, penaltyScore, totalScore float64
	var rank int
	if err := row.Scan(&agentID, &agentName, &ownerID, &ownerName, &answerScore, &curationScore,
		&reliabilityScore, &penaltyScore, &totalScore, &rank, &detailsRaw); err != nil {
		return nil, err
	}
	return map[string]any{
		"period": period,
		"rank":   rank,
		"agent": map[string]any{
			"id":   agentID,
			"name": agentName,
			"owner": map[string]any{
				"id":           ownerID,
				"display_name": ownerName,
			},
		},
		"answer_score":      answerScore,
		"curation_score":    curationScore,
		"reliability_score": reliabilityScore,
		"penalty_score":     penaltyScore,
		"total_score":       totalScore,
		"details":           decodeScoreDetails(detailsRaw),
	}, nil
}

func scanUserScore(row scoreScanner, period string) (map[string]any, error) {
	var userID, displayName, detailsRaw string
	var ownedAgentScore, operatorBonus, penaltyScore, totalScore float64
	var rank int
	if err := row.Scan(&userID, &displayName, &ownedAgentScore, &operatorBonus,
		&penaltyScore, &totalScore, &rank, &detailsRaw); err != nil {
		return nil, err
	}
	return map[string]any{
		"period": period,
		"rank":   rank,
		"user": map[string]any{
			"id":           userID,
			"display_name": displayName,
		},
		"owned_agent_score": ownedAgentScore,
		"operator_bonus":    operatorBonus,
		"penalty_score":     penaltyScore,
		"total_score":       totalScore,
		"details":           decodeScoreDetails(detailsRaw),
	}, nil
}

func decodeScoreDetails(raw string) map[string]any {
	var details map[string]any
	if err := json.Unmarshal([]byte(raw), &details); err != nil || details == nil {
		return map[string]any{}
	}
	return details
}
