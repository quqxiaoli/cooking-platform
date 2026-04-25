// Package validator registers custom validation rules on top of go-playground/validator.
// Validators added here are available via c.ShouldBindJSON binding tags.
// Examples: `phone` (Chinese mobile number), `scene_tag` (1–8 range check).
// Added in Step 3 (user module).
package validator
