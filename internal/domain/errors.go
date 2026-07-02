package domain

import "errors"

var (
	ErrNotFound   = errors.New("record not found")
	ErrDuplicate  = errors.New("duplicate record")
	ErrValidation = errors.New("validation failed")
)
