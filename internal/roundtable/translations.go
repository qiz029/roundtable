package roundtable

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
)

const translationVersion = 1

var supportedTranslationLanguages = []string{"en", "zh-CN"}

type TranslationProvider interface {
	Translate(ctx context.Context, req TranslationProviderRequest) (TranslationProviderResult, error)
}

type TranslationProviderRequest struct {
	ResourceType   string
	ResourceID     string
	SourceLanguage string
	TargetLanguage string
	Title          string
	Body           string
}

type TranslationProviderResult struct {
	Title        string
	Body         string
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	CostMicros   int
}

type TranslationWorkerConfig struct {
	Enabled             bool
	PollInterval        time.Duration
	BatchSize           int
	MaxConcurrency      int
	MaxAttempts         int
	RetryBaseDelay      time.Duration
	DailyBudgetMicros   int
	EstimatedCostMicros int
}

type translatableResource struct {
	Type           string
	ID             string
	Title          string
	Body           string
	SourceLanguage string
	SourceHash     string
}

type translationRecord struct {
	ID                 string
	ResourceType       string
	ResourceID         string
	TargetLanguage     string
	SourceHash         string
	TranslationVersion int
	TranslatedTitle    string
	TranslatedBody     string
	Provider           string
	Model              string
	InputTokens        int
	OutputTokens       int
	CostMicros         int
	CreatedAt          string
	UpdatedAt          string
}

type translationJob struct {
	ID                 string
	ResourceType       string
	ResourceID         string
	TargetLanguage     string
	SourceHash         string
	TranslationVersion int
	Attempts           int
	MaxAttempts        int
}

func normalizeTranslationWorkerConfig(config TranslationWorkerConfig) TranslationWorkerConfig {
	if config.PollInterval <= 0 {
		config.PollInterval = 30 * time.Second
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 10
	}
	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = 1
	}
	if config.MaxAttempts <= 0 {
		config.MaxAttempts = 3
	}
	if config.RetryBaseDelay <= 0 {
		config.RetryBaseDelay = time.Minute
	}
	return config
}

func validLanguage(language string) bool {
	for _, supported := range supportedTranslationLanguages {
		if language == supported {
			return true
		}
	}
	return false
}

func validResourceType(resourceType string) bool {
	return resourceType == "question" || resourceType == "answer"
}

func detectTranslationSourceLanguage(title string, body string) string {
	for _, r := range title + "\n" + body {
		if unicode.Is(unicode.Han, r) {
			return "zh-CN"
		}
	}
	return "en"
}

func (a *App) startTranslationWorker() {
	if a.translationProvider == nil || !a.translationWorker.Enabled {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.translationCancel = cancel
	a.translationDone = make(chan struct{})
	go func() {
		defer close(a.translationDone)
		ticker := time.NewTicker(a.translationWorker.PollInterval)
		defer ticker.Stop()
		for {
			_, err := a.ProcessTranslationJobs(ctx, a.translationWorker.BatchSize)
			if err != nil && a.logger != nil && !errors.Is(err, context.Canceled) {
				a.logger.Warn("translation_worker_failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func translationSourceHash(resourceType string, title string, body string) string {
	sum := sha256.Sum256([]byte(resourceType + "\x00" + title + "\x00" + body))
	return hex.EncodeToString(sum[:])
}

func (a *App) loadTranslatableResource(ctx context.Context, resourceType string, resourceID string) (translatableResource, error) {
	resource := translatableResource{Type: resourceType, ID: resourceID}
	var err error
	switch resourceType {
	case "question":
		err = a.db.QueryRowContext(ctx, `
			SELECT q.title, q.body
			FROM questions q
			JOIN users author ON author.id = q.author_user_id
			WHERE q.id = $1 AND author.status = 'active'
		`, resourceID).Scan(&resource.Title, &resource.Body)
	case "answer":
		err = a.db.QueryRowContext(ctx, `
			SELECT ans.body
			FROM answers ans
			JOIN questions q ON q.id = ans.question_id
			JOIN users question_author ON question_author.id = q.author_user_id
			JOIN agents ag ON ag.id = ans.agent_id
			JOIN users owner ON owner.id = ag.owner_user_id
			WHERE ans.id = $1
				AND question_author.status = 'active'
				AND owner.status = 'active'
		`, resourceID).Scan(&resource.Body)
	default:
		return translatableResource{}, errInvalidInput("resource_type must be question or answer")
	}
	if err != nil {
		return translatableResource{}, err
	}
	resource.SourceLanguage = detectTranslationSourceLanguage(resource.Title, resource.Body)
	resource.SourceHash = translationSourceHash(resource.Type, resource.Title, resource.Body)
	return resource, nil
}

func (a *App) translationByCacheKey(ctx context.Context, resource translatableResource, targetLanguage string) (translationRecord, error) {
	var record translationRecord
	err := a.db.QueryRowContext(ctx, `
		SELECT id, resource_type, resource_id, target_language, source_hash, translation_version,
			translated_title, translated_body, provider, model, input_tokens, output_tokens,
			cost_micros, created_at, updated_at
		FROM content_translations
		WHERE resource_type = $1
			AND resource_id = $2
			AND target_language = $3
			AND source_hash = $4
			AND translation_version = $5
	`, resource.Type, resource.ID, targetLanguage, resource.SourceHash, translationVersion).
		Scan(&record.ID, &record.ResourceType, &record.ResourceID, &record.TargetLanguage,
			&record.SourceHash, &record.TranslationVersion, &record.TranslatedTitle,
			&record.TranslatedBody, &record.Provider, &record.Model, &record.InputTokens,
			&record.OutputTokens, &record.CostMicros, &record.CreatedAt, &record.UpdatedAt)
	return record, err
}

func (a *App) enqueueTranslationJob(ctx context.Context, resource translatableResource, targetLanguage string) error {
	if !validResourceType(resource.Type) {
		return errInvalidInput("resource_type must be question or answer")
	}
	if !validLanguage(targetLanguage) {
		return errInvalidInput("target_language must be en or zh-CN")
	}
	if resource.SourceLanguage == targetLanguage {
		return nil
	}
	jobID, err := newID("trj")
	if err != nil {
		return err
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(ctx, `
		INSERT INTO translation_jobs (
			id, resource_type, resource_id, target_language, source_hash, translation_version,
			status, attempts, max_attempts, next_attempt_at, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', 0, $7, $8, $8, $8)
		ON CONFLICT (resource_type, resource_id, target_language, source_hash, translation_version)
		DO NOTHING
	`, jobID, resource.Type, resource.ID, targetLanguage, resource.SourceHash,
		translationVersion, a.translationWorker.MaxAttempts, now)
	return err
}

func (a *App) enqueueTranslationJobsForResource(ctx context.Context, resourceType string, resourceID string) error {
	resource, err := a.loadTranslatableResource(ctx, resourceType, resourceID)
	if err != nil {
		return err
	}
	for _, language := range supportedTranslationLanguages {
		if resource.SourceLanguage == language {
			continue
		}
		if err := a.enqueueTranslationJob(ctx, resource, language); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) enqueueTranslationJobsBestEffort(ctx context.Context, resourceType string, resourceID string) {
	if err := a.enqueueTranslationJobsForResource(ctx, resourceType, resourceID); err != nil && a.logger != nil {
		a.logger.Warn("enqueue_translation_jobs_failed", "resource_type", resourceType, "resource_id", resourceID, "error", err)
	}
}

func (a *App) enqueueTranslationBackfill(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `
		SELECT q.id
		FROM questions q
		JOIN users author ON author.id = q.author_user_id
		WHERE author.status = 'active'
	`)
	if err != nil {
		return err
	}
	questionIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		questionIDs = append(questionIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range questionIDs {
		if err := a.enqueueTranslationJobsForResource(ctx, "question", id); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}

	rows, err = a.db.QueryContext(ctx, `
		SELECT ans.id
		FROM answers ans
		JOIN questions q ON q.id = ans.question_id
		JOIN users question_author ON question_author.id = q.author_user_id
		JOIN agents ag ON ag.id = ans.agent_id
		JOIN users owner ON owner.id = ag.owner_user_id
		WHERE question_author.status = 'active'
			AND owner.status = 'active'
	`)
	if err != nil {
		return err
	}
	answerIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		answerIDs = append(answerIDs, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range answerIDs {
		if err := a.enqueueTranslationJobsForResource(ctx, "answer", id); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

func (a *App) ProcessTranslationJobs(ctx context.Context, limit int) (int, error) {
	if a.translationProvider == nil {
		return 0, nil
	}
	if limit <= 0 || limit > a.translationWorker.BatchSize {
		limit = a.translationWorker.BatchSize
	}
	jobs, err := a.pendingTranslationJobs(ctx, limit)
	if err != nil {
		return 0, err
	}
	if a.translationWorker.MaxConcurrency > 1 && a.translationWorker.DailyBudgetMicros <= 0 {
		return a.processTranslationJobsConcurrently(ctx, jobs)
	}
	return a.processTranslationJobsSequential(ctx, jobs)
}

func (a *App) processTranslationJobsSequential(ctx context.Context, jobs []translationJob) (int, error) {
	processed := 0
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		didProcess, err := a.processTranslationJob(ctx, job)
		if err != nil {
			return processed, err
		}
		if didProcess {
			processed++
		}
	}
	return processed, nil
}

func (a *App) processTranslationJobsConcurrently(ctx context.Context, jobs []translationJob) (int, error) {
	maxConcurrency := a.translationWorker.MaxConcurrency
	if maxConcurrency > len(jobs) {
		maxConcurrency = len(jobs)
	}
	if maxConcurrency <= 1 {
		return a.processTranslationJobsSequential(ctx, jobs)
	}

	type result struct {
		processed bool
		err       error
	}
	sem := make(chan struct{}, maxConcurrency)
	results := make(chan result, len(jobs))
	started := 0
startJobs:
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break startJobs
		}
		started++
		go func(job translationJob) {
			defer func() { <-sem }()
			didProcess, err := a.processTranslationJob(ctx, job)
			results <- result{processed: didProcess, err: err}
		}(job)
	}

	processed := 0
	var firstErr error
	for i := 0; i < started; i++ {
		result := <-results
		if result.processed {
			processed++
		}
		if firstErr == nil && result.err != nil {
			firstErr = result.err
		}
	}
	if firstErr != nil {
		return processed, firstErr
	}
	return processed, ctx.Err()
}

func (a *App) pendingTranslationJobs(ctx context.Context, limit int) ([]translationJob, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT id, resource_type, resource_id, target_language, source_hash, translation_version,
			attempts, max_attempts
		FROM translation_jobs
		WHERE status = 'pending'
			AND next_attempt_at <= $1
		ORDER BY created_at ASC
		LIMIT $2
	`, a.now().UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := []translationJob{}
	for rows.Next() {
		var job translationJob
		if err := rows.Scan(&job.ID, &job.ResourceType, &job.ResourceID, &job.TargetLanguage,
			&job.SourceHash, &job.TranslationVersion, &job.Attempts, &job.MaxAttempts); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (a *App) processTranslationJob(ctx context.Context, job translationJob) (bool, error) {
	now := a.now().UTC()
	result, err := a.db.ExecContext(ctx, `
		UPDATE translation_jobs
		SET status = 'running', locked_at = $1, updated_at = $1
		WHERE id = $2 AND status = 'pending'
	`, now.Format(time.RFC3339Nano), job.ID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return false, nil
	}

	resource, err := a.loadTranslatableResource(ctx, job.ResourceType, job.ResourceID)
	if err != nil || resource.SourceHash != job.SourceHash {
		message := "resource is no longer public or current"
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			message = err.Error()
		}
		return true, a.failTranslationJob(ctx, job, message)
	}
	if resource.SourceLanguage == job.TargetLanguage {
		return true, a.markTranslationJobSucceeded(ctx, job, TranslationProviderResult{})
	}
	if _, err := a.translationByCacheKey(ctx, resource, job.TargetLanguage); err == nil {
		return true, a.markTranslationJobSucceeded(ctx, job, TranslationProviderResult{})
	} else if !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	if ok, err := a.translationBudgetAllows(ctx); err != nil {
		return true, err
	} else if !ok {
		return true, a.deferTranslationJobForBudget(ctx, job)
	}

	providerResult, err := a.translationProvider.Translate(ctx, TranslationProviderRequest{
		ResourceType:   resource.Type,
		ResourceID:     resource.ID,
		SourceLanguage: resource.SourceLanguage,
		TargetLanguage: job.TargetLanguage,
		Title:          resource.Title,
		Body:           resource.Body,
	})
	if err != nil {
		return true, a.retryOrFailTranslationJob(ctx, job, err.Error())
	}
	if providerResult.Provider == "" {
		providerResult.Provider = "unknown"
	}
	if providerResult.Model == "" {
		providerResult.Model = "unknown"
	}
	if err := a.storeTranslation(ctx, resource, job.TargetLanguage, providerResult); err != nil {
		return true, err
	}
	return true, a.markTranslationJobSucceeded(ctx, job, providerResult)
}

func (a *App) storeTranslation(ctx context.Context, resource translatableResource, targetLanguage string, result TranslationProviderResult) error {
	translationID, err := newID("trs")
	if err != nil {
		return err
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(ctx, `
		INSERT INTO content_translations (
			id, resource_type, resource_id, target_language, source_hash, translation_version,
			translated_title, translated_body, provider, model, input_tokens, output_tokens,
			cost_micros, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
		ON CONFLICT (resource_type, resource_id, target_language, source_hash, translation_version)
		DO NOTHING
	`, translationID, resource.Type, resource.ID, targetLanguage, resource.SourceHash,
		translationVersion, strings.TrimSpace(result.Title), strings.TrimSpace(result.Body),
		result.Provider, result.Model, result.InputTokens, result.OutputTokens, result.CostMicros, now)
	return err
}

func (a *App) markTranslationJobSucceeded(ctx context.Context, job translationJob, result TranslationProviderResult) error {
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err := a.db.ExecContext(ctx, `
		UPDATE translation_jobs
		SET status = 'succeeded',
			provider = $1,
			model = $2,
			input_tokens = $3,
			output_tokens = $4,
			cost_micros = $5,
			last_error = '',
			updated_at = $6
		WHERE id = $7
	`, result.Provider, result.Model, result.InputTokens, result.OutputTokens, result.CostMicros, now, job.ID)
	return err
}

func (a *App) retryOrFailTranslationJob(ctx context.Context, job translationJob, message string) error {
	if job.Attempts+1 >= job.MaxAttempts {
		return a.failTranslationJob(ctx, job, message)
	}
	now := a.now().UTC()
	nextAttempt := now.Add(time.Duration(job.Attempts+1) * a.translationWorker.RetryBaseDelay)
	_, err := a.db.ExecContext(ctx, `
		UPDATE translation_jobs
		SET status = 'pending',
			attempts = attempts + 1,
			next_attempt_at = $1,
			last_error = $2,
			updated_at = $3
		WHERE id = $4
	`, nextAttempt.Format(time.RFC3339Nano), message, now.Format(time.RFC3339Nano), job.ID)
	return err
}

func (a *App) failTranslationJob(ctx context.Context, job translationJob, message string) error {
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err := a.db.ExecContext(ctx, `
		UPDATE translation_jobs
		SET status = 'failed',
			attempts = attempts + 1,
			last_error = $1,
			updated_at = $2
		WHERE id = $3
	`, message, now, job.ID)
	return err
}

func (a *App) deferTranslationJobForBudget(ctx context.Context, job translationJob) error {
	now := a.now().UTC()
	nextAttempt := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	_, err := a.db.ExecContext(ctx, `
		UPDATE translation_jobs
		SET status = 'pending',
			next_attempt_at = $1,
			last_error = 'translation budget exhausted',
			updated_at = $2
		WHERE id = $3
	`, nextAttempt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), job.ID)
	return err
}

func (a *App) translationBudgetAllows(ctx context.Context) (bool, error) {
	if a.translationWorker.DailyBudgetMicros <= 0 {
		return true, nil
	}
	now := a.now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	var spent int
	if err := a.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0)
		FROM translation_jobs
		WHERE status = 'succeeded' AND updated_at >= $1
	`, dayStart).Scan(&spent); err != nil {
		return false, err
	}
	if a.translationWorker.EstimatedCostMicros > 0 {
		return spent+a.translationWorker.EstimatedCostMicros <= a.translationWorker.DailyBudgetMicros, nil
	}
	return spent < a.translationWorker.DailyBudgetMicros, nil
}

func translationRecordResponse(resource translatableResource, record translationRecord) map[string]any {
	return map[string]any{
		"status":              "ready",
		"resource_type":       record.ResourceType,
		"resource_id":         record.ResourceID,
		"source_language":     resource.SourceLanguage,
		"target_language":     record.TargetLanguage,
		"source_hash":         record.SourceHash,
		"translation_version": record.TranslationVersion,
		"translation": map[string]any{
			"title": record.TranslatedTitle,
			"body":  record.TranslatedBody,
		},
		"provider":      record.Provider,
		"model":         record.Model,
		"input_tokens":  record.InputTokens,
		"output_tokens": record.OutputTokens,
		"cost_micros":   record.CostMicros,
		"created_at":    record.CreatedAt,
		"updated_at":    record.UpdatedAt,
	}
}

func pendingTranslationResponse(resource translatableResource, targetLanguage string) map[string]any {
	return map[string]any{
		"status":              "pending",
		"resource_type":       resource.Type,
		"resource_id":         resource.ID,
		"source_language":     resource.SourceLanguage,
		"target_language":     targetLanguage,
		"source_hash":         resource.SourceHash,
		"translation_version": translationVersion,
	}
}

func originalTranslationResponse(resource translatableResource, targetLanguage string) map[string]any {
	return map[string]any{
		"status":              "ready",
		"resource_type":       resource.Type,
		"resource_id":         resource.ID,
		"source_language":     resource.SourceLanguage,
		"target_language":     targetLanguage,
		"source_hash":         resource.SourceHash,
		"translation_version": translationVersion,
		"translation": map[string]any{
			"title": resource.Title,
			"body":  resource.Body,
		},
		"provider":      "",
		"model":         "",
		"input_tokens":  0,
		"output_tokens": 0,
		"cost_micros":   0,
		"created_at":    "",
		"updated_at":    "",
	}
}
