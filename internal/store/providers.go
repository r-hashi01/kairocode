package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// ProviderPref is what is kept of a provider: the model and effort last
// chosen, and the models and efforts its agent said it has.
type ProviderPref struct {
	Model, Effort   string
	Models, Efforts []string
}

// ProviderPref is provider's (zero if nothing is kept).
func (s *Store) ProviderPref(ctx context.Context, provider string) (ProviderPref, error) {
	var p ProviderPref
	var models, efforts string
	err := s.db.QueryRowContext(ctx, `SELECT model, effort, models, efforts FROM kc_provider_pref WHERE provider = ?`, provider).
		Scan(&p.Model, &p.Effort, &models, &efforts)
	if errors.Is(err, sql.ErrNoRows) {
		return ProviderPref{}, nil
	}
	if err != nil {
		return ProviderPref{}, err
	}
	_ = json.Unmarshal([]byte(models), &p.Models)
	_ = json.Unmarshal([]byte(efforts), &p.Efforts)
	return p, nil
}

// LastProvider is the provider last chosen, in any session ("" if none).
// It is kept in the row of provider "", in its model.
func (s *Store) LastProvider(ctx context.Context) (string, error) {
	p, err := s.ProviderPref(ctx, "")
	return p.Model, err
}

// SetLastProvider keeps provider as the one last chosen.
func (s *Store) SetLastProvider(ctx context.Context, provider string) error {
	return s.SetProviderChoice(ctx, "", provider, "")
}

// SetProviderChoice keeps the model and the effort chosen for provider
// ("" leaves one as it is).
func (s *Store) SetProviderChoice(ctx context.Context, provider, model, effort string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_provider_pref (provider, model, effort) VALUES (?, ?, ?)
		ON CONFLICT (provider) DO UPDATE SET
			model = CASE WHEN excluded.model = '' THEN kc_provider_pref.model ELSE excluded.model END,
			effort = CASE WHEN excluded.effort = '' THEN kc_provider_pref.effort ELSE excluded.effort END`,
		provider, model, effort)
	return err
}

// SetProviderOptions keeps the models and efforts provider's agent said
// it has (nil leaves one as it is).
func (s *Store) SetProviderOptions(ctx context.Context, provider string, models, efforts []string) error {
	m, _ := json.Marshal(models)
	e, _ := json.Marshal(efforts)
	_, err := s.db.ExecContext(ctx, `INSERT INTO kc_provider_pref (provider, models, efforts) VALUES (?, ?, ?)
		ON CONFLICT (provider) DO UPDATE SET
			models = CASE WHEN excluded.models = 'null' THEN kc_provider_pref.models ELSE excluded.models END,
			efforts = CASE WHEN excluded.efforts = 'null' THEN kc_provider_pref.efforts ELSE excluded.efforts END`,
		provider, string(m), string(e))
	return err
}
