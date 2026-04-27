// Package validator — phone.go registers the "phone" tag for go-playground/validator.
//
// Usage in DTOs:
//
//	type SendCodeReq struct {
//	    Phone string `json:"phone" binding:"required,phone"`
//	}
//
// The validator is registered once at startup via Register(). Gin uses the
// global validator engine internally for c.ShouldBindJSON, so registration
// must happen before any handler binds a struct with the "phone" tag.
package validator

import (
	"errors"
	"regexp"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

// chinaMobilePattern matches mainland China mobile numbers.
//
// Accepted prefixes (as of 2025): 13x, 14x, 15x, 16x, 17x, 18x, 19x where x
// is any digit. This covers all carriers (China Mobile, Unicom, Telecom,
// Broadnet) and IoT/special segments.
//
// We deliberately do NOT support international numbers (+86 prefix, country
// code variations). Internationalisation is a Phase 2 product decision, not
// an MVP requirement (PRD-Phase1: 22-28 year old domestic users).
var chinaMobilePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)

// errInvalidValidatorEngine is returned when gin's validator engine is not
// the expected go-playground/validator type. Sentinel error for the unlikely
// scenario where gin's internals change.
var errInvalidValidatorEngine = errors.New("gin validator engine is not *validator.Validate")

// Register installs all custom validators on Gin's default validator engine.
//
// Called once from main.go after gin.SetMode but before any route is mounted.
// Idempotent: safe to call multiple times (validator/v10 silently overwrites
// duplicate tag registrations).
func Register() error {
	v, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		// gin uses go-playground/validator internally; this should never fail.
		// If it does, the gin version is incompatible — fail loudly at startup.
		return errInvalidValidatorEngine
	}

	if err := v.RegisterValidation("phone", validatePhone); err != nil {
		return err
	}
	return nil
}

// validatePhone is the actual matcher invoked by go-playground/validator.
func validatePhone(fl validator.FieldLevel) bool {
	return chinaMobilePattern.MatchString(fl.Field().String())
}
