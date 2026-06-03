// Package domain contains core domain types and sentinel errors for the payment service.
package domain

import "errors"

// Sentinel errors for the payment domain.
var (
	ErrNotFound            = errors.New("not found")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrForbidden           = errors.New("forbidden")
	ErrValidation          = errors.New("validation error")
	ErrConflict            = errors.New("conflict")
	ErrTransactionNotFound = errors.New("transaction not found")
	ErrInvalidTransition   = errors.New("invalid state transition")
	ErrDuplicateKey        = errors.New("duplicate idempotency key")
)
