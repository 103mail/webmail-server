// Package auth issues and validates the bearer tokens the API uses. It is the
// Go port of AuthApi and MintyPHP's Auth::login.
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/103mail/webmail-server/internal/store"
)

// Identity is the caller a bearer token resolves to. A zero CustomerID means
// the request is unauthenticated.
type Identity struct {
	CustomerID int64
	UserID     int64
}

// Authenticated reports whether the identity may act on the API.
func (i Identity) Authenticated() bool { return i.CustomerID != 0 }

// User is the account behind a token, as returned by GET /auth/token.
type User struct {
	UserID     int64  `json:"userId"`
	CustomerID int64  `json:"customerId"`
	Email      string `json:"email"`
	Name       string `json:"name"`
}

// Token is the credential returned by POST /auth/token.
type Token struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ErrInvalidCredentials is returned when the email and password do not match an
// account.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Service reads and writes tokens.
type Service struct {
	db *store.DB
}

// New returns a Service backed by db.
func New(db *store.DB) *Service { return &Service{db: db} }

// Authorize resolves an Authorization header to an identity. An absent,
// malformed or unknown token yields the zero Identity rather than an error, so
// handlers uniformly answer 401.
func (s *Service) Authorize(ctx context.Context, header string) (Identity, error) {
	value, ok := bearer(header)
	if !ok {
		return Identity{}, nil
	}
	var id Identity
	var tokenID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT "id", "customer_id", "user_id" FROM "token" WHERE "value" = ?`,
		value).Scan(&tokenID, &id.CustomerID, &id.UserID)
	if err == sql.ErrNoRows {
		return Identity{}, nil
	}
	if err != nil {
		return Identity{}, fmt.Errorf("look up token: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "token" SET "used_at" = ? WHERE "id" = ?`, s.db.Now(), tokenID); err != nil {
		return Identity{}, fmt.Errorf("record token use: %w", err)
	}
	return id, nil
}

// bearer extracts the credential from an "Authorization: Bearer x" header.
func bearer(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	value := strings.TrimSpace(header[len(prefix):])
	return value, value != ""
}

// CurrentUser returns the account for an identity.
func (s *Service) CurrentUser(ctx context.Context, id Identity) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT "id", "customer_id", "username", "name" FROM "user" WHERE "customer_id" = ? AND "id" = ?`,
		id.CustomerID, id.UserID).Scan(&u.UserID, &u.CustomerID, &u.Email, &u.Name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up user: %w", err)
	}
	return &u, nil
}

// Login verifies the password and returns a token with the given name, reusing
// the existing token of that name when there is one.
func (s *Service) Login(ctx context.Context, name, email, password string) (*Token, error) {
	if name == "" {
		name = "Session"
	}
	email = store.Address(email)
	var userID, customerID int64
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT "id", "customer_id", "password" FROM "user" WHERE "username" = ? LIMIT 1`,
		email).Scan(&userID, &customerID, &hash)
	if err == sql.ErrNoRows {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("look up user: %w", err)
	}
	if hash == "" {
		// Accounts created by external onboarding have no password yet.
		return nil, ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrInvalidCredentials
	}

	var token *Token
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		tokenID, err := store.SelectInt(ctx, tx,
			`SELECT "id" FROM "token" WHERE "user_id" = ? AND "name" = ?`, userID, name)
		if err != nil {
			return fmt.Errorf("look up token: %w", err)
		}
		if tokenID == 0 {
			value, err := newTokenValue()
			if err != nil {
				return err
			}
			// The customer id comes from the user record. The PHP reads it from
			// the token row that does not exist yet, which cannot work on a
			// first login because token.customer_id is NOT NULL.
			tokenID, err = store.Insert(ctx, tx,
				`INSERT INTO "token" ("customer_id", "user_id", "name", "value") VALUES (?, ?, ?, ?)`,
				customerID, userID, name, value)
			if err != nil {
				return fmt.Errorf("create token: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "token" SET "validated_at" = ? WHERE "id" = ?`, s.db.Now(), tokenID); err != nil {
			return fmt.Errorf("validate token: %w", err)
		}
		var t Token
		if err := tx.QueryRowContext(ctx,
			`SELECT "name", "value" FROM "token" WHERE "id" = ?`, tokenID).Scan(&t.Name, &t.Value); err != nil {
			return fmt.Errorf("read token: %w", err)
		}
		token = &t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return token, nil
}

func newTokenValue() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Account is a user and every address that reaches its customer's mailboxes.
type Account struct {
	UserID     int64    `json:"userId"`
	CustomerID int64    `json:"customerId"`
	Username   string   `json:"username"`
	Addresses  []string `json:"addresses"`
}

// Accounts lists every user on this Server with their addresses, sorted.
func (s *Service) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT "id", "customer_id", "username" FROM "user" ORDER BY "id"`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	accounts := []Account{}
	for rows.Next() {
		a := Account{Addresses: []string{}}
		if err := rows.Scan(&a.UserID, &a.CustomerID, &a.Username); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan user: %w", err)
		}
		accounts = append(accounts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	rows, err = s.db.QueryContext(ctx,
		`SELECT "customer_id", "email" FROM "alias" ORDER BY "email"`)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()
	byCustomer := map[int64][]string{}
	for rows.Next() {
		var customerID int64
		var address string
		if err := rows.Scan(&customerID, &address); err != nil {
			return nil, fmt.Errorf("scan alias: %w", err)
		}
		byCustomer[customerID] = append(byCustomer[customerID], address)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	for i := range accounts {
		if addresses := byCustomer[accounts[i].CustomerID]; addresses != nil {
			accounts[i].Addresses = addresses
		}
	}
	return accounts, nil
}
