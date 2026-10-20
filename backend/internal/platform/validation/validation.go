// Package validation carries field-level validation failures from domain services to the HTTP
// layer, which renders them as RFC 9457 problems with fieldErrors (422).
package validation

import "strings"

// FieldError is one failed rule on one request field.
type FieldError struct {
	// Field is the dotted path of the field in the request body, for example "url".
	Field   string
	Message string
}

// Error collects field errors. The zero value is ready to use.
type Error struct {
	Fields []FieldError
}

func (e *Error) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// Add records a failure for field.
func (e *Error) Add(field, message string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: message})
}

// Err returns e when it holds at least one failure, otherwise nil.
func (e *Error) Err() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}
