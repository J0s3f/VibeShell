package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"j0s.at/vibeshell/internal/domain"
)

// RecordCommand associates a visible command name with the app that
// implements it. A name may move to a later app (re-recording is last-wins,
// exactly like the in-memory index the shell used before this table existed),
// but only to an app that is registered, so the index never points at an
// application storage does not know.
func (a *Apps) RecordCommand(ctx context.Context, name string, app domain.AppID) error {
	if name == "" || app.IsZero() {
		return domain.NewValidationError(
			domain.CodeInvalidInput, "command name and app are required", nil)
	}
	tx, err := a.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin record command: %w", err)
	}
	defer tx.Rollback()

	var registered int
	switch err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM apps WHERE app_id = ?`, app.String()).Scan(&registered); {
	case errors.Is(err, sql.ErrNoRows):
		return domain.NewNotFoundError(
			domain.CodeAppNotFound, "app is not registered",
			map[string]string{"app": app.String()},
		)
	case err != nil:
		return fmt.Errorf("sqlite: read app %q: %w", app.String(), err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO app_commands(command, app_id)
		VALUES(?,?)
		ON CONFLICT(command) DO UPDATE SET app_id = excluded.app_id`,
		name, app.String(),
	); err != nil {
		return fmt.Errorf("sqlite: record command %q: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit record command: %w", err)
	}
	return nil
}

// CommandIndex returns every recorded command name with the app that
// implements it. It is the full routing table the shell rebuilds at startup
// so a command accepted by an earlier process resolves to its stored app
// instead of generating a duplicate.
func (a *Apps) CommandIndex(ctx context.Context) (map[string]domain.AppID, error) {
	rows, err := a.sql.QueryContext(ctx, `SELECT command, app_id FROM app_commands ORDER BY command`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read app command index: %w", err)
	}
	defer rows.Close()
	out := make(map[string]domain.AppID)
	for rows.Next() {
		var name, appID string
		if err := rows.Scan(&name, &appID); err != nil {
			return nil, fmt.Errorf("sqlite: read app command index: %w", err)
		}
		app, err := domain.ParseAppID(appID)
		if err != nil {
			return nil, decodeFailure("command index app id", err)
		}
		out[name] = app
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read app command index: %w", err)
	}
	return out, nil
}
